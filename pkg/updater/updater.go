package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultGitHubAPIBase = "https://api.github.com"
	EnvPortableExe       = "APPINSTALLER_PORTABLE_EXE"
	EnvLegacyLauncherExe = "TOWNCRIER_LAUNCHER_EXE"
	ManifestFileName     = "install-manifest.json"
)

var (
	ErrChecksumFileMissing = errors.New("release does not contain checksums.txt or SHA256SUMS.txt")
	ErrChecksumEntryNotFound = errors.New("target asset not found in release checksums file")
	ErrChecksumMismatch    = errors.New("downloaded asset SHA-256 checksum mismatch")
	ErrNoMatchingAsset     = errors.New("no matching release asset found for current platform")
)

// InstallMode describes how the application is currently installed/running on the host.
type InstallMode string

const (
	ModeInstalledWindows InstallMode = "installed_windows"
	ModePortableWindows  InstallMode = "portable_windows"
	ModePortableUnix     InstallMode = "portable_unix"
)

// Asset represents a downloadable file attached to a GitHub Release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// ReleaseInfo represents metadata for a GitHub Release.
type ReleaseInfo struct {
	TagName     string    `json:"tag_name"`
	Version     string    `json:"-"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Checker queries GitHub Releases and downloads/verifies release assets.
type Checker struct {
	Owner      string
	Repo       string
	APIBaseURL string
	HTTPClient *http.Client
	UserAgent  string
}

// NewChecker creates a new Checker for the given GitHub owner and repository.
func NewChecker(owner, repo string) *Checker {
	return &Checker{
		Owner:      owner,
		Repo:       repo,
		APIBaseURL: DefaultGitHubAPIBase,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		UserAgent:  repo + "-Updater/1.0",
	}
}

// NormalizeVersion strips any leading 'v' or 'V' and whitespace from a version string.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return strings.TrimSpace(v)
}

// CompareVersions compares two semantic version strings (e.g. "v0.0.7" vs "0.0.8").
// Returns 1 if v1 > v2, -1 if v1 < v2, and 0 if equal.
func CompareVersions(v1, v2 string) int {
	n1 := NormalizeVersion(v1)
	n2 := NormalizeVersion(v2)
	if n1 == n2 {
		return 0
	}

	core1, pre1, _ := strings.Cut(n1, "-")
	core2, pre2, _ := strings.Cut(n2, "-")

	parts1 := strings.Split(core1, ".")
	parts2 := strings.Split(core2, ".")
	maxLen := max(len(parts1), len(parts2))

	for i := range maxLen {
		var p1, p2 int
		if i < len(parts1) {
			p1, _ = strconv.Atoi(parts1[i])
		}
		if i < len(parts2) {
			p2, _ = strconv.Atoi(parts2[i])
		}
		if p1 > p2 {
			return 1
		}
		if p1 < p2 {
			return -1
		}
	}

	// A release without a pre-release suffix is newer than one with a pre-release suffix (e.g. 1.0.0 > 1.0.0-rc1)
	if pre1 == "" && pre2 != "" {
		return 1
	}
	if pre1 != "" && pre2 == "" {
		return -1
	}
	return strings.Compare(pre1, pre2)
}

// CheckLatest fetches the latest published release from GitHub and reports whether it is newer than currentVersion.
func (c *Checker) CheckLatest(ctx context.Context, currentVersion string) (*ReleaseInfo, bool, error) {
	base := strings.TrimRight(c.APIBaseURL, "/")
	if base == "" {
		base = DefaultGitHubAPIBase
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", base, c.Owner, c.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("github api returned HTTP %d", resp.StatusCode)
	}

	var rel ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, false, fmt.Errorf("decode release json: %w", err)
	}
	rel.Version = NormalizeVersion(rel.TagName)
	if rel.Version == "" {
		return nil, false, errors.New("latest release has empty tag_name")
	}

	isNewer := CompareVersions(rel.Version, currentVersion) > 0
	return &rel, isNewer, nil
}

// ResolveTargetExecutable returns the outer executable path that should be updated.
// When running inside a self-extracting portable launcher, it returns the outer launcher's path.
func ResolveTargetExecutable(currentExe string) string {
	if p := strings.TrimSpace(os.Getenv(EnvPortableExe)); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv(EnvLegacyLauncherExe)); p != "" {
		return p
	}
	if currentExe != "" {
		return currentExe
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// DetectInstallMode inspects environment variables and the executable's directory to determine InstallMode.
func DetectInstallMode(exePath string) InstallMode {
	return DetectInstallModeForOS(exePath, runtime.GOOS)
}

// DetectInstallModeForOS determines InstallMode for the given OS and executable path.
func DetectInstallModeForOS(exePath, goos string) InstallMode {
	if strings.TrimSpace(os.Getenv(EnvPortableExe)) != "" || strings.TrimSpace(os.Getenv(EnvLegacyLauncherExe)) != "" {
		if goos == "windows" {
			return ModePortableWindows
		}
		return ModePortableUnix
	}

	if exePath == "" {
		exePath, _ = os.Executable()
	}
	dir := filepath.Dir(exePath)

	if goos == "windows" {
		if _, err := os.Stat(filepath.Join(dir, ManifestFileName)); err == nil {
			return ModeInstalledWindows
		}
		if _, err := os.Stat(filepath.Join(dir, "uninstall.exe")); err == nil {
			return ModeInstalledWindows
		}
		return ModePortableWindows
	}

	return ModePortableUnix
}

// SelectAsset picks the appropriate release asset for the given InstallMode, OS, and architecture.
func SelectAsset(rel *ReleaseInfo, mode InstallMode, goos, goarch string) (*Asset, error) {
	if rel == nil || len(rel.Assets) == 0 {
		return nil, ErrNoMatchingAsset
	}

	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))

	isArchMatch := func(nameLower string) bool {
		if strings.Contains(nameLower, goarch) {
			return true
		}
		if goos == "darwin" && strings.Contains(nameLower, "all") {
			return true
		}
		return false
	}

	// 1. If ModeInstalledWindows, prefer the *-setup.exe / *_setup.exe installer asset
	if mode == ModeInstalledWindows {
		for i := range rel.Assets {
			a := &rel.Assets[i]
			n := strings.ToLower(a.Name)
			if strings.Contains(n, "windows") && isArchMatch(n) && (strings.HasSuffix(n, "-setup.exe") || strings.HasSuffix(n, "_setup.exe")) {
				return a, nil
			}
		}
	}

	// 2. Portable Windows or fallback if setup.exe is not present
	if goos == "windows" {
		for i := range rel.Assets {
			a := &rel.Assets[i]
			n := strings.ToLower(a.Name)
			if strings.Contains(n, "inkscape-extension") || strings.HasSuffix(n, "setup.exe") {
				continue
			}
			if strings.Contains(n, "windows") && isArchMatch(n) && (strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".exe")) {
				return a, nil
			}
		}
		return nil, ErrNoMatchingAsset
	}

	// 3. Unix (Linux / macOS) portable archives
	for i := range rel.Assets {
		a := &rel.Assets[i]
		n := strings.ToLower(a.Name)
		if strings.Contains(n, "inkscape-extension") {
			continue
		}
		if strings.Contains(n, goos) && isArchMatch(n) &&
			(strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".zip")) {
			return a, nil
		}
	}

	return nil, ErrNoMatchingAsset
}

// ParseChecksums parses a standard sha256sum / checksums.txt / SHA256SUMS.txt file into a map of filename -> lowercase hex sha256.
func ParseChecksums(content string) map[string]string {
	out := make(map[string]string)
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := strings.ToLower(strings.TrimSpace(fields[0]))
		if len(hash) != 64 {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(fields[1]), "*")
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		out[name] = hash
	}
	return out
}

func findChecksumAsset(rel *ReleaseInfo) *Asset {
	if rel == nil {
		return nil
	}
	for i := range rel.Assets {
		n := strings.ToLower(rel.Assets[i].Name)
		if n == "checksums.txt" || n == "sha256sums.txt" || strings.HasSuffix(n, "_checksums.txt") {
			return &rel.Assets[i]
		}
	}
	return nil
}

// DownloadAndVerify downloads the release's checksum file, streams the target asset to destDir,
// and verifies its SHA-256 digest before returning the local file path.
func (c *Checker) DownloadAndVerify(ctx context.Context, rel *ReleaseInfo, asset *Asset, destDir string, progress func(downloaded, total int64)) (string, error) {
	if rel == nil || asset == nil {
		return "", errors.New("release and asset must not be nil")
	}

	csAsset := findChecksumAsset(rel)
	if csAsset == nil {
		return "", ErrChecksumFileMissing
	}

	csBytes, err := c.fetchBytes(ctx, csAsset.BrowserDownloadURL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", csAsset.Name, err)
	}

	checksums := ParseChecksums(string(csBytes))
	expectedHash, ok := checksums[asset.Name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrChecksumEntryNotFound, asset.Name)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("create download directory: %w", err)
	}

	targetPath := filepath.Join(destDir, asset.Name)
	tmpPath := targetPath + ".part"
	_ = os.Remove(tmpPath)

	actualHash, err := c.downloadFileWithHash(ctx, asset.BrowserDownloadURL, tmpPath, asset.Size, progress)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	if !strings.EqualFold(actualHash, expectedHash) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, expectedHash, actualHash)
	}

	_ = os.Remove(targetPath)
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("finalize downloaded file: %w", err)
	}

	return targetPath, nil
}

func (c *Checker) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
}

func (c *Checker) downloadFileWithHash(ctx context.Context, url, dstPath string, expectedSize int64, progress func(downloaded, total int64)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	} else if client.Timeout > 0 && client.Timeout < 5*time.Minute {
		clientCopy := *client
		clientCopy.Timeout = 10 * time.Minute
		client = &clientCopy
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download asset: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download asset returned HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if total <= 0 {
		total = expectedSize
	}

	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", err
	}

	hasher := sha256.New()
	buf := make([]byte, 64*1024)
	var downloaded int64

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, wErr := f.Write(chunk); wErr != nil {
				_ = f.Close()
				return "", wErr
			}
			_, _ = hasher.Write(chunk)
			downloaded += int64(n)
			if progress != nil {
				progress(downloaded, total)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			_ = f.Close()
			return "", readErr
		}
	}

	if err := f.Close(); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ReplacePortableBinary extracts the updated binary from verifiedAssetPath (if it is a .zip or .tar.gz)
// or copies it directly, and atomically replaces targetExePath by rotating the current binary to <targetExePath>.old.
func ReplacePortableBinary(verifiedAssetPath, targetExePath string) error {
	if targetExePath == "" {
		return errors.New("target executable path cannot be empty")
	}

	newExePath := targetExePath + ".new"
	_ = os.Remove(newExePath)

	lowerAsset := strings.ToLower(verifiedAssetPath)
	expectedExeName := filepath.Base(targetExePath)

	var extractErr error
	switch {
	case strings.HasSuffix(lowerAsset, ".zip"):
		extractErr = extractExeFromZip(verifiedAssetPath, expectedExeName, newExePath)
	case strings.HasSuffix(lowerAsset, ".tar.gz") || strings.HasSuffix(lowerAsset, ".tgz"):
		extractErr = extractExeFromTarGz(verifiedAssetPath, expectedExeName, newExePath)
	default:
		extractErr = copyExecutableFile(verifiedAssetPath, newExePath)
	}
	if extractErr != nil {
		_ = os.Remove(newExePath)
		return extractErr
	}

	oldExePath := targetExePath + ".old"
	_ = os.Remove(oldExePath)

	if _, statErr := os.Stat(targetExePath); statErr == nil {
		if err := os.Rename(targetExePath, oldExePath); err != nil {
			_ = os.Remove(newExePath)
			return fmt.Errorf("rotate existing binary to .old: %w", err)
		}
	}

	if err := os.Rename(newExePath, targetExePath); err != nil {
		// Rollback if possible
		_ = os.Rename(oldExePath, targetExePath)
		_ = os.Remove(newExePath)
		return fmt.Errorf("activate new binary: %w", err)
	}

	return nil
}

func copyExecutableFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func extractExeFromZip(zipPath, expectedName, dstPath string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var chosen *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if strings.EqualFold(base, expectedName) {
			chosen = f
			break
		}
		if chosen == nil && strings.HasSuffix(strings.ToLower(base), ".exe") && !strings.Contains(strings.ToLower(base), "uninstall") {
			chosen = f
		}
	}
	if chosen == nil {
		return fmt.Errorf("executable %s not found in zip archive", expectedName)
	}

	rc, err := chosen.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, rc); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func extractExeFromTarGz(tarGzPath, expectedName, dstPath string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return fmt.Errorf("open tar.gz archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if strings.EqualFold(base, expectedName) {
			dst, openErr := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if openErr != nil {
				return openErr
			}
			if _, copyErr := io.Copy(dst, tr); copyErr != nil {
				_ = dst.Close()
				return copyErr
			}
			return dst.Close()
		}
	}
	return fmt.Errorf("executable %s not found in tar.gz archive", expectedName)
}

// ApplyAndRestart applies the verified update asset and restarts the application.
func ApplyAndRestart(verifiedAssetPath, targetExePath string, mode InstallMode, onQuit func()) error {
	targetExePath = ResolveTargetExecutable(targetExePath)
	if targetExePath == "" {
		return errors.New("could not resolve target executable path")
	}

	lowerAsset := strings.ToLower(verifiedAssetPath)
	isSetupExe := strings.HasSuffix(lowerAsset, "-setup.exe") || strings.HasSuffix(lowerAsset, "_setup.exe")

	if mode == ModeInstalledWindows && isSetupExe {
		installDir := filepath.Dir(targetExePath)
		cmd := exec.Command(
			verifiedAssetPath,
			"--silent",
			"--dir", installDir,
			"--wait-pid", strconv.Itoa(os.Getpid()),
			"--relaunch",
		)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("launch silent updater: %w", err)
		}
		if onQuit != nil {
			onQuit()
		}
		return nil
	}

	if err := ReplacePortableBinary(verifiedAssetPath, targetExePath); err != nil {
		return err
	}

	cmd := exec.Command(targetExePath)
	cmd.Dir = filepath.Dir(targetExePath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relaunch updated binary: %w", err)
	}

	if onQuit != nil {
		onQuit()
	}
	return nil
}
