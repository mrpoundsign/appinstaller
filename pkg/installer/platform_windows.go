//go:build windows

package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const uninstallRegistryBase = `Software\Microsoft\Windows\CurrentVersion\Uninstall`

type windowsRegistryManager struct{}

func newPlatformRegistryManager() RegistryManager {
	return windowsRegistryManager{}
}

func (windowsRegistryManager) RegisterUninstall(reg UninstallRegistration) error {
	if reg.AppName == "" {
		return nil
	}
	keyPath := uninstallRegistryBase + `\` + reg.AppName
	k, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create registry key %s: %w", keyPath, err)
	}
	defer func() { _ = k.Close() }()

	_ = k.SetStringValue("DisplayName", reg.AppName)
	_ = k.SetStringValue("DisplayVersion", reg.Version)
	_ = k.SetStringValue("Publisher", reg.Publisher)
	_ = k.SetStringValue("InstallLocation", reg.InstallLocation)
	_ = k.SetStringValue("DisplayIcon", reg.DisplayIcon)
	_ = k.SetStringValue("UninstallString", reg.UninstallString)
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	if reg.EstimatedSizeKB > 0 {
		_ = k.SetDWordValue("EstimatedSize", reg.EstimatedSizeKB)
	}
	return nil
}

func (windowsRegistryManager) UnregisterUninstall(appName string) error {
	if appName == "" {
		return nil
	}
	keyPath := uninstallRegistryBase + `\` + appName
	err := registry.DeleteKey(registry.CURRENT_USER, keyPath)
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

func (windowsRegistryManager) AddToUserPath(dir string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" {
		return nil
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()

	current, _, _ := k.GetStringValue("Path")
	parts := strings.Split(current, ";")
	for _, p := range parts {
		if strings.EqualFold(filepath.Clean(strings.TrimSpace(p)), dir) {
			return nil
		}
	}
	var updated string
	if strings.TrimSpace(current) == "" {
		updated = dir
	} else {
		updated = strings.TrimRight(current, ";") + ";" + dir
	}
	return k.SetExpandStringValue("Path", updated)
}

func (windowsRegistryManager) RemoveFromUserPath(dir string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" {
		return nil
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()

	current, _, err := k.GetStringValue("Path")
	if err != nil {
		return nil
	}
	parts := strings.Split(current, ";")
	var kept []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if !strings.EqualFold(filepath.Clean(trimmed), dir) {
			kept = append(kept, trimmed)
		}
	}
	return k.SetExpandStringValue("Path", strings.Join(kept, ";"))
}

type windowsShortcutManager struct{}

func newPlatformShortcutManager() ShortcutManager {
	return windowsShortcutManager{}
}

func (windowsShortcutManager) StartMenuShortcutPath(appName string) (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", errors.New("APPDATA environment variable is not set")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", appName+".lnk"), nil
}

func (windowsShortcutManager) DesktopShortcutPath(appName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Desktop", appName+".lnk"), nil
}

func (windowsShortcutManager) CreateShortcut(shortcutPath, targetPath, workingDir, description string) error {
	if err := os.MkdirAll(filepath.Dir(shortcutPath), 0755); err != nil {
		return err
	}
	esc := func(s string) string {
		return strings.ReplaceAll(s, "'", "''")
	}
	script := fmt.Sprintf(
		"$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%s'); $s.TargetPath = '%s'; $s.WorkingDirectory = '%s'; $s.Description = '%s'; $s.IconLocation = '%s,0'; $s.Save()",
		esc(shortcutPath),
		esc(targetPath),
		esc(workingDir),
		esc(description),
		esc(targetPath),
	)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

// WaitForPID waits for the specified Windows process ID to exit or until timeout expires.
func WaitForPID(ctx context.Context, pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		event, waitErr := windows.WaitForSingleObject(handle, 250)
		if waitErr != nil || event == windows.WAIT_OBJECT_0 {
			time.Sleep(150 * time.Millisecond)
			return
		}
	}
}

// ScheduleSelfDelete spawns a detached background cmd.exe process to delete uninstall.exe and remove installDir after process exit.
func ScheduleSelfDelete(installDir, uninstallerPath string) {
	cmdline := fmt.Sprintf(`ping 127.0.0.1 -n 2 > NUL & del /f /q "%s" & rmdir "%s"`, uninstallerPath, installDir)
	cmd := exec.Command("cmd.exe", "/C", cmdline)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
