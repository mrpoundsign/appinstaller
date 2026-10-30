//go:build !windows

package installer

import (
	"context"
	"os"
	"syscall"
	"time"
)

func newPlatformRegistryManager() RegistryManager {
	return NoopRegistryManager{}
}

func newPlatformShortcutManager() ShortcutManager {
	return NoopShortcutManager{}
}

// WaitForPID waits for the specified process ID to exit or until timeout expires.
func WaitForPID(ctx context.Context, pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ScheduleSelfDelete is a no-op on non-Windows platforms where open binaries can be unlinked directly.
func ScheduleSelfDelete(_, uninstallerPath string) {
	_ = os.Remove(uninstallerPath)
}
