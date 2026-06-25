//go:build !windows

package system

func hostMemoryBytes() int64 {
	return 0
}
