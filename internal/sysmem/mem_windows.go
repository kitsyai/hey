//go:build windows

package sysmem

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	globalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct.
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

func available() (uint64, error) {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	ret, _, err := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if ret == 0 {
		return 0, fmt.Errorf("sysmem: GlobalMemoryStatusEx: %w", err)
	}
	return st.AvailPhys, nil
}
