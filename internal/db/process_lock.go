package db

import (
	"fmt"
	"os"
)

// acquireProcessLock locks a stable sidecar file rather than the database inode.
// The sidecar remains the same across compaction's rename-based file swap.
func acquireProcessLock(dbPath string) (*os.File, error) {
	lockFile, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database lock: %w", err)
	}
	if err := lockFileExclusive(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("database is already open by another process (or lock failed): %w", err)
	}
	return lockFile, nil
}

func releaseProcessLock(f *os.File) error {
	if f == nil {
		return nil
	}
	unlockErr := unlockFile(lockFilePlatformHandle(f))
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
