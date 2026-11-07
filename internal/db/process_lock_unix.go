//go:build !windows

package db

import (
	"os"
	"syscall"
)

func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
func lockFilePlatformHandle(f *os.File) *os.File { return f }
func unlockFile(f *os.File) error                { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
