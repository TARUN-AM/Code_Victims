//go:build windows

package db

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32Lock     = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32Lock.NewProc("LockFileEx")
	procUnlockFileEx = kernel32Lock.NewProc("UnlockFileEx")
)

type lockOverlapped struct {
	Internal, InternalHigh uintptr
	Offset, OffsetHigh     uint32
	HEvent                 uintptr
}

func lockFileExclusive(f *os.File) error {
	var overlapped lockOverlapped
	r, _, e := procLockFileEx.Call(f.Fd(), 0x00000002|0x00000001, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r == 0 {
		return e
	}
	return nil
}
func lockFilePlatformHandle(f *os.File) *os.File { return f }
func unlockFile(f *os.File) error {
	var overlapped lockOverlapped
	r, _, e := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r == 0 {
		return e
	}
	return nil
}
