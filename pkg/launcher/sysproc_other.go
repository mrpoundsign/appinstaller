//go:build !windows

package launcher

import "os/exec"

func setSysProcAttr(_ *exec.Cmd) {}
