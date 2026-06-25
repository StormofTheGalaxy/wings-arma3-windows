//go:build windows

package system

import (
	"syscall"
	"unsafe"
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func hostMemoryBytes() int64 {
	var mem memoryStatusEx
	mem.Length = uint32(unsafe.Sizeof(mem))
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	r1, _, _ := proc.Call(uintptr(unsafe.Pointer(&mem)))
	if r1 == 0 {
		return 0
	}
	return int64(mem.TotalPhys)
}
