//go:build !windows

package windows

import (
	"context"
	"os"
)

func killProcessTree(_ context.Context, pid int, _ bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
