package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const ManifestFileName = "install-manifest.json"

// ComponentSpec defines a project-specific optional installation step or hook
// (e.g., pre-downloading TTS models, installing an Inkscape extension, or adding CLI to PATH).
type ComponentSpec struct {
	ID          string
	Title       string
	Description string
	DefaultOn   bool
	Run         func(ctx context.Context, installDir string, progress func(cur, total int64, status string)) error
}

// AppSpec declaratively describes an application for the unified installer and uninstaller.
type AppSpec struct {
	AppName            string
	AppID              string
	Description        string
	Publisher          string
	Version            string
	RepoOwner          string
	RepoName           string
	BinaryName         string
	PayloadFS          fs.FS
	PayloadSubdir      string
	FileRenames        map[string]string
	DataDirsToPurge    func() []string
	OptionalComponents []ComponentSpec
}

// Options holds the user-selected configuration for an installation run.
type Options struct {
	InstallDir              string
	CreateStartMenuShortcut bool
	CreateDesktopShortcut   bool
	AddToUserPath           bool
	SelectedComponents      map[string]bool
	WaitPID                 int
	SelfExePath             string
}

// Manifest is persisted as install-manifest.json in the installation directory.
type Manifest struct {
	AppName         string    `json:"app_name"`
	AppID           string    `json:"app_id"`
	Version         string    `json:"version"`
	InstallDir      string    `json:"install_dir"`
	BinaryName      string    `json:"binary_name"`
	InstalledFiles  []string  `json:"installed_files"`
	Shortcuts       []string  `json:"shortcuts,omitempty"`
	AddedToUserPath bool      `json:"added_to_user_path,omitempty"`
	InstalledAt     time.Time `json:"installed_at"`
}

// UninstallRegistration holds metadata written to Windows "Installed Apps" (HKCU Uninstall registry).
type UninstallRegistration struct {
	AppName          string
	AppID            string
	Version          string
	Publisher        string
	InstallLocation  string
	DisplayIcon      string
	UninstallString  string
	EstimatedSizeKB  uint32
}

// RegistryManager abstracts OS registry and user PATH operations for testability.
type RegistryManager interface {
	RegisterUninstall(reg UninstallRegistration) error
	UnregisterUninstall(appName string) error
	AddToUserPath(dir string) error
	RemoveFromUserPath(dir string) error
}

// ShortcutManager abstracts desktop and Start Menu shortcut creation.
type ShortcutManager interface {
	StartMenuShortcutPath(appName string) (string, error)
	DesktopShortcutPath(appName string) (string, error)
	CreateShortcut(shortcutPath, targetPath, workingDir, description string) error
}

// NoopRegistryManager is a safe in-memory no-op RegistryManager used in non-Windows builds and unit tests.
type NoopRegistryManager struct{}

func (NoopRegistryManager) RegisterUninstall(UninstallRegistration) error { return nil }
func (NoopRegistryManager) UnregisterUninstall(string) error              { return nil }
func (NoopRegistryManager) AddToUserPath(string) error                    { return nil }
func (NoopRegistryManager) RemoveFromUserPath(string) error               { return nil }

// NoopShortcutManager is a safe in-memory no-op ShortcutManager used in non-Windows builds and unit tests.
type NoopShortcutManager struct {
	BaseDir string
}

func (m NoopShortcutManager) StartMenuShortcutPath(appName string) (string, error) {
	if m.BaseDir == "" {
		return "", nil
	}
	return filepath.Join(m.BaseDir, "StartMenu", appName+".lnk"), nil
}

func (m NoopShortcutManager) DesktopShortcutPath(appName string) (string, error) {
	if m.BaseDir == "" {
		return "", nil
	}
	return filepath.Join(m.BaseDir, "Desktop", appName+".lnk"), nil
}

func (m NoopShortcutManager) CreateShortcut(shortcutPath, targetPath, _, _ string) error {
	if shortcutPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(shortcutPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(shortcutPath, []byte(targetPath), 0644)
}

// DefaultInstallDir returns the default per-user installation directory (%LOCALAPPDATA%\Programs\<AppName>).
func DefaultInstallDir(appName string) string {
	if appName == "" {
		appName = "App"
	}
	if runtime.GOOS == "windows" {
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			return filepath.Join(localApp, "Programs", appName)
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, "AppData", "Local", "Programs", appName)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", strings.ToLower(appName))
	}
	return filepath.Join(os.TempDir(), appName)
}

// Installer executes installation and uninstallation workflows for an AppSpec.
type Installer struct {
	Spec      AppSpec
	Registry  RegistryManager
	Shortcuts ShortcutManager
}

// NewInstaller constructs an Installer with platform-appropriate Registry and Shortcut managers.
// During unit tests (testing.Testing()), it defaults to Noop managers to guarantee zero host mutation.
func NewInstaller(spec AppSpec) *Installer {
	if spec.PayloadSubdir == "" {
		spec.PayloadSubdir = "payload"
	}
	if spec.Publisher == "" {
		spec.Publisher = "mrpoundsign"
	}
	if testing.Testing() {
		return &Installer{
			Spec:      spec,
			Registry:  NoopRegistryManager{},
			Shortcuts: NoopShortcutManager{},
		}
	}
	return &Installer{
		Spec:      spec,
		Registry:  newPlatformRegistryManager(),
		Shortcuts: newPlatformShortcutManager(),
	}
}

func ensureSafePathInTests(targetDir string) error {
	if !testing.Testing() {
		return nil
	}
	tempDir := filepath.Clean(os.TempDir())
	cleanTarget := filepath.Clean(targetDir)
	rel, err := filepath.Rel(tempDir, cleanTarget)
	isTemp := err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
	if !isTemp {
		return fmt.Errorf("refusing to modify %q outside os.TempDir() during unit tests", targetDir)
	}
	return nil
}

// Install unpacks the embedded payload to opts.InstallDir, writes uninstall.exe and install-manifest.json,
// creates shortcuts and registry entries, and executes any selected OptionalComponents.
func (ins *Installer) Install(ctx context.Context, opts Options, progress func(fraction float64, status string)) (*Manifest, error) {
	if opts.InstallDir == "" {
		opts.InstallDir = DefaultInstallDir(ins.Spec.AppName)
	}
	if err := ensureSafePathInTests(opts.InstallDir); err != nil {
		return nil, err
	}

	report := func(frac float64, msg string) {
		if progress != nil {
			progress(min(max(frac, 0.0), 1.0), msg)
		}
	}

	if opts.WaitPID > 0 {
		report(0.02, "Waiting for running application instance to close...")
		WaitForPID(ctx, opts.WaitPID, 15*time.Second)
	}

	report(0.05, "Creating installation directory...")
	if err := os.MkdirAll(opts.InstallDir, 0755); err != nil {
		return nil, fmt.Errorf("create install dir: %w", err)
	}

	var installedFiles []string
	var totalBytes int64

	if ins.Spec.PayloadFS != nil {
		entries, err := collectPayloadFiles(ins.Spec.PayloadFS, ins.Spec.PayloadSubdir)
		if err != nil {
			return nil, fmt.Errorf("scan embedded payload: %w", err)
		}

		for i, relPath := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			dstRel := relPath
			if mapped, ok := ins.Spec.FileRenames[relPath]; ok && mapped != "" {
				dstRel = mapped
			} else if mappedBase, ok := ins.Spec.FileRenames[filepath.Base(relPath)]; ok && mappedBase != "" {
				dstRel = filepath.Join(filepath.Dir(relPath), mappedBase)
			}

			frac := 0.05 + 0.45*(float64(i)/float64(max(len(entries), 1)))
			report(frac, "Installing "+dstRel+"...")

			embPath := ins.Spec.PayloadSubdir + "/" + relPath
			dstFull := filepath.Join(opts.InstallDir, filepath.FromSlash(dstRel))

			n, copyErr := extractFileFromFS(ins.Spec.PayloadFS, embPath, dstFull)
			if copyErr != nil {
				return nil, fmt.Errorf("install %s: %w", dstRel, copyErr)
			}
			totalBytes += n
			installedFiles = append(installedFiles, filepath.ToSlash(dstRel))
		}
	}

	// Copy the installer executable itself as uninstall.exe
	selfExe := opts.SelfExePath
	if selfExe == "" && !testing.Testing() {
		selfExe, _ = os.Executable()
	}
	uninstallerName := "uninstall.exe"
	if runtime.GOOS != "windows" {
		uninstallerName = "uninstall"
	}
	uninstallerPath := filepath.Join(opts.InstallDir, uninstallerName)
	if selfExe != "" && !strings.EqualFold(filepath.Clean(selfExe), filepath.Clean(uninstallerPath)) {
		if _, err := os.Stat(selfExe); err == nil {
			report(0.52, "Creating uninstaller...")
			if n, err := copyDiskFile(selfExe, uninstallerPath); err == nil {
				totalBytes += n
				if !slices.Contains(installedFiles, uninstallerName) {
					installedFiles = append(installedFiles, uninstallerName)
				}
			}
		}
	}

	mainExePath := filepath.Join(opts.InstallDir, ins.Spec.BinaryName)
	var createdShortcuts []string

	if ins.Shortcuts != nil {
		if opts.CreateStartMenuShortcut {
			if smPath, err := ins.Shortcuts.StartMenuShortcutPath(ins.Spec.AppName); err == nil && smPath != "" {
				report(0.55, "Creating Start Menu shortcut...")
				if err := ins.Shortcuts.CreateShortcut(smPath, mainExePath, opts.InstallDir, ins.Spec.Description); err == nil {
					createdShortcuts = append(createdShortcuts, smPath)
				}
			}
		}
		if opts.CreateDesktopShortcut {
			if dtPath, err := ins.Shortcuts.DesktopShortcutPath(ins.Spec.AppName); err == nil && dtPath != "" {
				report(0.58, "Creating Desktop shortcut...")
				if err := ins.Shortcuts.CreateShortcut(dtPath, mainExePath, opts.InstallDir, ins.Spec.Description); err == nil {
					createdShortcuts = append(createdShortcuts, dtPath)
				}
			}
		}
	}

	if ins.Registry != nil {
		report(0.60, "Registering application with operating system...")
		_ = ins.Registry.RegisterUninstall(UninstallRegistration{
			AppName:         ins.Spec.AppName,
			AppID:           ins.Spec.AppID,
			Version:         ins.Spec.Version,
			Publisher:       ins.Spec.Publisher,
			InstallLocation: opts.InstallDir,
			DisplayIcon:     mainExePath,
			UninstallString: fmt.Sprintf(`"%s" --uninstall`, uninstallerPath),
			EstimatedSizeKB: uint32(max(totalBytes/1024, 1)),
		})
		if opts.AddToUserPath {
			_ = ins.Registry.AddToUserPath(opts.InstallDir)
		}
	}

	manifest := &Manifest{
		AppName:         ins.Spec.AppName,
		AppID:           ins.Spec.AppID,
		Version:         ins.Spec.Version,
		InstallDir:      opts.InstallDir,
		BinaryName:      ins.Spec.BinaryName,
		InstalledFiles:  installedFiles,
		Shortcuts:       createdShortcuts,
		AddedToUserPath: opts.AddToUserPath,
		InstalledAt:     time.Now().UTC(),
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal install manifest: %w", err)
	}
	manifestPath := filepath.Join(opts.InstallDir, ManifestFileName)
	if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
		return nil, fmt.Errorf("write install manifest: %w", err)
	}

	// Execute any selected OptionalComponents (e.g. TTS model pre-downloads, Inkscape extensions)
	var activeComponents []ComponentSpec
	for _, comp := range ins.Spec.OptionalComponents {
		if opts.SelectedComponents != nil && opts.SelectedComponents[comp.ID] && comp.Run != nil {
			activeComponents = append(activeComponents, comp)
		}
	}

	for idx, comp := range activeComponents {
		baseFrac := 0.65 + 0.33*(float64(idx)/float64(len(activeComponents)))
		spanFrac := 0.33 / float64(len(activeComponents))
		report(baseFrac, comp.Title+"...")
		err := comp.Run(ctx, opts.InstallDir, func(cur, total int64, status string) {
			subRatio := 0.0
			if total > 0 {
				subRatio = min(float64(cur)/float64(total), 1.0)
			}
			msg := status
			if msg == "" {
				msg = comp.Title
			}
			report(baseFrac+spanFrac*subRatio, msg)
		})
		if err != nil {
			return manifest, fmt.Errorf("component %q failed: %w", comp.Title, err)
		}
	}

	report(1.0, "Installation complete!")
	return manifest, nil
}

// LoadManifest reads install-manifest.json from the given installation directory.
func LoadManifest(installDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(installDir, ManifestFileName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Uninstall removes installed files, shortcuts, registry keys, and optionally user data directories.
func (ins *Installer) Uninstall(ctx context.Context, installDir string, purgeUserData bool, progress func(fraction float64, status string)) error {
	if installDir == "" {
		installDir = DefaultInstallDir(ins.Spec.AppName)
	}
	if err := ensureSafePathInTests(installDir); err != nil {
		return err
	}

	report := func(frac float64, msg string) {
		if progress != nil {
			progress(min(max(frac, 0.0), 1.0), msg)
		}
	}

	report(0.10, "Reading installation manifest...")
	manifest, _ := LoadManifest(installDir)

	if manifest != nil {
		report(0.25, "Removing application shortcuts...")
		for _, sc := range manifest.Shortcuts {
			if sc != "" {
				_ = os.Remove(sc)
			}
		}
		if manifest.AddedToUserPath && ins.Registry != nil {
			_ = ins.Registry.RemoveFromUserPath(installDir)
		}

		report(0.50, "Removing installed application files...")
		for _, rel := range manifest.InstalledFiles {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Skip deleting the currently running uninstaller binary until the very end
			if strings.EqualFold(rel, "uninstall.exe") || strings.EqualFold(rel, "uninstall") {
				continue
			}
			target := filepath.Join(installDir, filepath.FromSlash(rel))
			_ = os.Remove(target)
		}
	} else {
		// Fallback if manifest is missing: remove main binary and known shortcuts
		if ins.Shortcuts != nil {
			if sm, err := ins.Shortcuts.StartMenuShortcutPath(ins.Spec.AppName); err == nil && sm != "" {
				_ = os.Remove(sm)
			}
			if dt, err := ins.Shortcuts.DesktopShortcutPath(ins.Spec.AppName); err == nil && dt != "" {
				_ = os.Remove(dt)
			}
		}
		if ins.Spec.BinaryName != "" {
			_ = os.Remove(filepath.Join(installDir, ins.Spec.BinaryName))
		}
	}

	_ = os.Remove(filepath.Join(installDir, ManifestFileName))

	if ins.Registry != nil {
		report(0.75, "Removing OS registry entries...")
		_ = ins.Registry.UnregisterUninstall(ins.Spec.AppName)
	}

	if purgeUserData && ins.Spec.DataDirsToPurge != nil {
		report(0.85, "Removing user configuration and cached data...")
		for _, dir := range ins.Spec.DataDirsToPurge() {
			if dir == "" {
				continue
			}
			if err := ensureSafePathInTests(dir); err != nil {
				continue
			}
			_ = os.RemoveAll(dir)
		}
	}

	uninstallerExe := filepath.Join(installDir, "uninstall.exe")
	selfExe, _ := os.Executable()
	if !testing.Testing() && runtime.GOOS == "windows" && strings.EqualFold(filepath.Clean(selfExe), filepath.Clean(uninstallerExe)) {
		ScheduleSelfDelete(installDir, uninstallerExe)
	} else {
		_ = os.Remove(uninstallerExe)
		_ = os.Remove(filepath.Join(installDir, "uninstall"))
		_ = os.Remove(installDir) // Removes directory if now empty
	}

	report(1.0, "Uninstallation complete!")
	return nil
}

func collectPayloadFiles(payloadFS fs.FS, subdir string) ([]string, error) {
	var files []string
	err := fs.WalkDir(payloadFS, subdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "placeholder.txt" {
			return nil
		}
		rel := strings.TrimPrefix(p, subdir+"/")
		if rel != "" && rel != p {
			files = append(files, rel)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return files, err
}

func extractFileFromFS(payloadFS fs.FS, embPath, dstPath string) (int64, error) {
	src, err := payloadFS.Open(embPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()

	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return 0, err
	}

	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(dst, src)
	if closeErr := dst.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return 0, copyErr
	}

	_ = os.Remove(dstPath)
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	return n, nil
}

func copyDiskFile(srcPath, dstPath string) (int64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(dst, src)
	if closeErr := dst.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	return n, copyErr
}
