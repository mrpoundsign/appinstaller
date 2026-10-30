package launcher

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
)

// Config defines the configuration for a single-file portable self-extracting launcher.
type Config struct {
	AppID          string
	AppVersion     string
	CoreBinaryName string // e.g. "towncrier-core" (".exe" appended automatically on Windows if omitted)
	PayloadFS      fs.FS
	PayloadSubdir  string // e.g. "payload"
}

// Run extracts the embedded payload to the versioned runtime directory, cleans up older version directories,
// and executes the core binary while forwarding signals and exit codes.
func Run(cfg Config) (int, error) {
	if cfg.PayloadSubdir == "" {
		cfg.PayloadSubdir = "payload"
	}

	if outerExe, err := os.Executable(); err == nil && outerExe != "" {
		CleanupOldPortableBinary(outerExe)
	}

	runtimeDir, err := GetRuntimeDir(cfg.AppID, cfg.AppVersion)
	if err != nil {
		return 1, fmt.Errorf("resolve runtime dir: %w", err)
	}

	if err := ExtractPayloadIfNeeded(cfg.PayloadFS, cfg.PayloadSubdir, runtimeDir); err != nil {
		return 1, fmt.Errorf("extract runtime payload: %w", err)
	}

	// Best-effort cleanup of older extracted runtime versions in the parent app directory
	_ = CleanupOldVersions(filepath.Dir(runtimeDir), cfg.AppVersion)

	runtime.GC()
	debug.FreeOSMemory()

	exeName := resolveCoreBinaryName(cfg.CoreBinaryName)
	coreExePath := filepath.Join(runtimeDir, exeName)
	if _, err := os.Stat(coreExePath); err != nil {
		return 1, fmt.Errorf("core executable not found at %s: %w", coreExePath, err)
	}

	cmd := exec.Command(coreExePath, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	outerExe, _ := os.Executable()
	env := os.Environ()
	if outerExe != "" {
		env = append(env,
			"APPINSTALLER_PORTABLE_EXE="+outerExe,
			"TOWNCRIER_LAUNCHER_EXE="+outerExe,
		)
	}

	if runtime.GOOS == "windows" {
		env = append(env, "PATH="+runtimeDir+";"+os.Getenv("PATH"))
	} else {
		ldPath := runtimeDir
		if existing := os.Getenv("LD_LIBRARY_PATH"); existing != "" {
			ldPath = runtimeDir + ":" + existing
		}
		dyldPath := runtimeDir
		if existing := os.Getenv("DYLD_LIBRARY_PATH"); existing != "" {
			dyldPath = runtimeDir + ":" + existing
		}
		env = append(env,
			"PATH="+runtimeDir+":"+os.Getenv("PATH"),
			"LD_LIBRARY_PATH="+ldPath,
			"DYLD_LIBRARY_PATH="+dyldPath,
		)
	}
	cmd.Env = env

	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("start %s: %w", exeName, err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-sigChan:
		return 0, nil
	case waitErr := <-done:
		if waitErr != nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
				return exitErr.ExitCode(), nil
			}
			return 1, waitErr
		}
		return 0, nil
	}
}

func resolveCoreBinaryName(base string) string {
	if base == "" {
		base = "towncrier-core"
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(base), ".exe") {
		return base + ".exe"
	}
	if runtime.GOOS != "windows" {
		return strings.TrimSuffix(base, ".exe")
	}
	return base
}

// GetRuntimeDir resolves the versioned runtime extraction directory under %LOCALAPPDATA% or UserCacheDir.
func GetRuntimeDir(appID, appVersion string) (string, error) {
	if appID == "" {
		appID = "com.github.mrpoundsign.app"
	}
	if appVersion == "" {
		appVersion = "0.0.0"
	}
	localApp := os.Getenv("LOCALAPPDATA")
	if localApp == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return filepath.Join(os.TempDir(), appID, "app", appVersion), nil
		}
		return filepath.Join(cacheDir, appID, "app", appVersion), nil
	}
	return filepath.Join(localApp, appID, "app", appVersion), nil
}

// CleanupOldVersions removes sibling version directories inside appBaseDir that do not match currentVersion.
func CleanupOldVersions(appBaseDir, currentVersion string) error {
	entries, err := os.ReadDir(appBaseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	normCurrent := strings.TrimPrefix(strings.TrimSpace(currentVersion), "v")
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		normName := strings.TrimPrefix(strings.TrimSpace(name), "v")
		if name == currentVersion || normName == normCurrent {
			continue
		}
		oldPath := filepath.Join(appBaseDir, name)
		_ = os.RemoveAll(oldPath)
	}
	return nil
}

// CleanupOldPortableBinary removes any leftover <exe>.old file created during a previous portable self-update.
func CleanupOldPortableBinary(exePath string) {
	if exePath == "" {
		return
	}
	oldPath := exePath + ".old"
	if _, err := os.Stat(oldPath); err == nil {
		_ = os.Remove(oldPath)
	}
}

// ExtractPayloadIfNeeded extracts embedded files from payloadFS/payloadSubdir into targetDir,
// skipping files whose size and SHA-256 digest already match on disk.
func ExtractPayloadIfNeeded(payloadFS fs.FS, payloadSubdir, targetDir string) error {
	entries, err := fs.ReadDir(payloadFS, payloadSubdir)
	if err != nil {
		return fmt.Errorf("read embedded payload: %w", err)
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".txt") {
			continue
		}

		targetPath := filepath.Join(targetDir, name)
		embeddedPath := payloadSubdir + "/" + name

		if MatchesExistingStream(payloadFS, targetPath, embeddedPath) {
			continue
		}

		if err := extractSingleFile(payloadFS, embeddedPath, targetPath); err != nil {
			return err
		}
	}

	return nil
}

func extractSingleFile(payloadFS fs.FS, embeddedPath, targetPath string) error {
	src, err := payloadFS.Open(embeddedPath)
	if err != nil {
		return fmt.Errorf("open embedded file %s: %w", embeddedPath, err)
	}
	defer func() { _ = src.Close() }()

	tmpPath := targetPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpPath, err)
	}

	if _, copyErr := io.Copy(dst, src); copyErr != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", tmpPath, copyErr)
	}
	if closeErr := dst.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, closeErr)
	}

	_ = os.Remove(targetPath)
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		if seeker, ok := src.(io.ReadSeeker); ok {
			if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr != nil {
				return fmt.Errorf("rename %s: %w", targetPath, err)
			}
			directDst, openErr := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if openErr != nil {
				return fmt.Errorf("replace %s: %w", targetPath, openErr)
			}
			_, copyErr := io.Copy(directDst, src)
			_ = directDst.Close()
			if copyErr != nil {
				return fmt.Errorf("replace %s: %w", targetPath, copyErr)
			}
			return nil
		}
		return fmt.Errorf("rename %s: %w", targetPath, err)
	}
	return nil
}

// MatchesExistingStream checks whether targetPath on disk has the exact same byte size and SHA-256 hash as embeddedPath in payloadFS.
func MatchesExistingStream(payloadFS fs.FS, targetPath, embeddedPath string) bool {
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return false
	}

	embFile, err := payloadFS.Open(embeddedPath)
	if err != nil {
		return false
	}
	defer func() { _ = embFile.Close() }()

	embInfo, err := embFile.Stat()
	if err != nil || targetInfo.Size() != embInfo.Size() {
		return false
	}

	existingFile, err := os.Open(targetPath)
	if err != nil {
		return false
	}
	defer func() { _ = existingFile.Close() }()

	h1 := sha256.New()
	if _, err := io.Copy(h1, embFile); err != nil {
		return false
	}

	h2 := sha256.New()
	if _, err := io.Copy(h2, existingFile); err != nil {
		return false
	}

	return string(h1.Sum(nil)) == string(h2.Sum(nil))
}
