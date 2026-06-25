//go:build windows

package windows

import (
	"context"
	"os/exec"
	"strconv"
)

func killProcessTree(ctx context.Context, pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	return exec.CommandContext(ctx, "taskkill.exe", args...).Run()
}
