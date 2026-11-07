package compaction

import (
	"fmt"
	"io"
	"os"

	"github.com/TARUN-AM/torq/internal/failpoint"
	"github.com/TARUN-AM/torq/internal/index"
	"github.com/TARUN-AM/torq/internal/limits"
	"github.com/TARUN-AM/torq/internal/recovery"
	"github.com/TARUN-AM/torq/internal/wal"
)

const (
	TmpSuffix = ".compact.tmp"
	BakSuffix = ".compact.bak"
)

// ResolveStartupResolution inspects the file matrix around dbPath:
// {dbPath, dbPath + ".compact.tmp", dbPath + ".compact.bak"}
// and deterministically resolves any mid-compaction crash (C1-C5).
func ResolveStartupResolution(dbPath string) error {
	tmpPath, bakPath := dbPath+TmpSuffix, dbPath+BakSuffix
	mainExists, tmpExists, bakExists := fileExists(dbPath), fileExists(tmpPath), fileExists(bakPath)
	if !tmpExists && !bakExists {
		return nil
	}

	mainValid := mainExists && isFileValid(dbPath)
	tmpValid := tmpExists && isFileValid(tmpPath)
	bakValid := bakExists && isFileValid(bakPath)

	// Prefer a fully validated main file. This is the completed-swap state;
	// stale artifacts can be removed only after the main copy is proven valid.
	if mainValid {
		if tmpExists {
			if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale compact temp: %w", err)
			}
		}
		if bakExists {
			if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale compact backup: %w", err)
			}
		}
		return nil
	}

	// Main is missing or invalid. Prefer a completely validated compacted
	// temp file; otherwise restore the last known-good backup.
	if tmpValid {
		if mainExists {
			if err := os.Remove(dbPath); err != nil {
				return fmt.Errorf("remove invalid main database: %w", err)
			}
		}
		if err := os.Rename(tmpPath, dbPath); err != nil {
			if bakValid {
				if restoreErr := os.Rename(bakPath, dbPath); restoreErr != nil {
					return fmt.Errorf("promote compact temp failed (%v), backup restore failed: %w", err, restoreErr)
				}
				return nil
			}
			return fmt.Errorf("promote validated compact temp: %w", err)
		}
		// The promoted file is validated; backup is now redundant.
		if bakExists {
			if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove old backup after promotion: %w", err)
			}
		}
		return nil
	}

	if bakValid {
		if mainExists {
			if err := os.Remove(dbPath); err != nil {
				return fmt.Errorf("remove invalid main database: %w", err)
			}
		}
		if err := os.Rename(bakPath, dbPath); err != nil {
			return fmt.Errorf("restore validated backup: %w", err)
		}
		if tmpExists {
			_ = os.Remove(tmpPath)
		}
		return nil
	}

	// No validated copy is available. Preserve all artifacts for manual
	// diagnosis; never delete the only remaining possible recovery material.
	return fmt.Errorf("no valid database copy found during compaction recovery (main=%t tmp=%t backup=%t)", mainValid, tmpValid, bakValid)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isFileValid(path string) bool {
	result, err := recoveryCheck(path)
	return err == nil && result
}

// recoveryCheck is isolated to avoid exposing the scanner implementation here.
func recoveryCheck(path string) (bool, error) {
	check, err := recovery.Check(path)
	if err != nil {
		return false, err
	}
	return check.Valid, nil
}

// writeFull handles short writes explicitly for compacted snapshot records.
func writeFull(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// CompactResult contains the newly constructed index and writer state.
type CompactResult struct {
	NewIndex  *index.Index
	NewOffset int64
	NextTxnID uint64
}

// ExecuteCompaction writes all live keys from oldIndex into a compacted file,
// flushes and syncs to disk, atomically swaps the files, and returns the new index and offset.
// This function must be executed under the database exclusive write lock.
func ExecuteCompaction(
	dbPath string,
	oldFile *os.File,
	oldIndex *index.Index,
	maxBytes int64,
) (*CompactResult, *os.File, error) {
	failpoint.Trigger("compaction_start")

	keys := oldIndex.Keys()

	// 0. Preflight size check for compacted database
	if maxBytes > 0 {
		transactionCount := (len(keys) + wal.MaxBatchSize - 1) / wal.MaxBatchSize
		expectedSize := int64(wal.FileHeaderSize) + int64(transactionCount)*limits.EncodedTxnOverhead
		for _, k := range keys {
			entry, ok := oldIndex.Get(k)
			if ok {
				expectedSize += limits.EncodedPutSize(len(k), int(entry.ValueLength))
			}
		}
		if expectedSize > maxBytes {
			return nil, nil, limits.ErrDiskFull
		}
	}

	tmpPath := dbPath + TmpSuffix
	bakPath := dbPath + BakSuffix

	// 1. Create temporary compaction file
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0666)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create compaction tmp file: %w", err)
	}

	// Clean up tmpFile on failure before swap
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// 2. Write 16-byte File Header
	hdrBuf := make([]byte, wal.FileHeaderSize)
	if err := wal.EncodeFileHeader(hdrBuf); err != nil {
		return nil, nil, fmt.Errorf("failed to encode file header: %w", err)
	}
	if err := writeFull(tmpFile, hdrBuf); err != nil {
		return nil, nil, fmt.Errorf("failed to write compaction file header: %w", err)
	}

	// 3. Stream the compacted snapshot as bounded committed transactions.
	// The normal database API caps batches at 10,000 operations; compaction
	// must preserve that invariant even when the database has millions of keys.
	newIdx := index.New()
	currentOffset := int64(wal.FileHeaderSize)
	nextTxnID := uint64(1)

	for chunkStart := 0; chunkStart < len(keys); chunkStart += wal.MaxBatchSize {
		chunkEnd := chunkStart + wal.MaxBatchSize
		if chunkEnd > len(keys) {
			chunkEnd = len(keys)
		}
		opCount := uint32(chunkEnd - chunkStart)

		beginBuf, err := wal.EncodeRecord(&wal.Record{Type: wal.RecordTypeBegin, TxnID: nextTxnID, OpCount: opCount})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to encode compaction BEGIN: %w", err)
		}
		if err := writeFull(tmpFile, beginBuf); err != nil {
			return nil, nil, fmt.Errorf("failed to write compaction BEGIN: %w", err)
		}
		currentOffset += int64(len(beginBuf))

		for _, k := range keys[chunkStart:chunkEnd] {
			entry, ok := oldIndex.Get(k)
			if !ok {
				return nil, nil, fmt.Errorf("index entry disappeared during compaction for key %q", k)
			}
			valBuf := make([]byte, entry.ValueLength)
			if entry.ValueLength > 0 {
				n, readErr := oldFile.ReadAt(valBuf, entry.ValueOffset)
				if readErr != nil {
					return nil, nil, fmt.Errorf("failed to read value for key %q at offset %d: %w", k, entry.ValueOffset, readErr)
				}
				if n != int(entry.ValueLength) {
					return nil, nil, fmt.Errorf("short read for key %q during compaction: expected %d, got %d", k, entry.ValueLength, n)
				}
			}

			recBuf, err := wal.EncodeRecord(&wal.Record{Type: wal.RecordTypePut, TxnID: nextTxnID, Key: []byte(k), Value: valBuf})
			if err != nil {
				return nil, nil, fmt.Errorf("failed to encode PUT record during compaction: %w", err)
			}
			recOffset := currentOffset
			if err := writeFull(tmpFile, recBuf); err != nil {
				return nil, nil, fmt.Errorf("failed to write PUT record during compaction: %w", err)
			}
			newIdx.Put(k, index.Entry{ValueOffset: recOffset + int64(wal.RecordHeaderSize+6+len(k)), ValueLength: entry.ValueLength})
			currentOffset += int64(len(recBuf))
		}

		commitBuf, err := wal.EncodeRecord(&wal.Record{Type: wal.RecordTypeCommit, TxnID: nextTxnID, OpCount: opCount})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to encode compaction COMMIT: %w", err)
		}
		if err := writeFull(tmpFile, commitBuf); err != nil {
			return nil, nil, fmt.Errorf("failed to write compaction COMMIT: %w", err)
		}
		currentOffset += int64(len(commitBuf))
		nextTxnID++
	}

	// Failpoint: before_sync
	failpoint.Trigger("compaction_before_sync")

	// 6. Fsync temporary file to durable storage
	if err := tmpFile.Sync(); err != nil {
		return nil, nil, fmt.Errorf("failed to sync compaction tmp file: %w", err)
	}
	_ = tmpFile.Close()

	// Failpoint: after_sync_before_rename
	failpoint.Trigger("compaction_after_sync_before_rename")

	// 7. Atomic Swap: close old file, rename main -> bak, rename tmp -> main, remove bak
	if err := oldFile.Close(); err != nil {
		return nil, nil, fmt.Errorf("failed to close old db file during compaction swap: %w", err)
	}

	if err := os.Rename(dbPath, bakPath); err != nil {
		return nil, nil, fmt.Errorf("failed to rename %s to %s: %w", dbPath, bakPath, err)
	}

	// Failpoint: after_rename_main_to_bak
	failpoint.Trigger("compaction_after_rename_main_to_bak")

	if err := os.Rename(tmpPath, dbPath); err != nil {
		// Roll back
		_ = os.Rename(bakPath, dbPath)
		return nil, nil, fmt.Errorf("failed to rename %s to %s: %w", tmpPath, dbPath, err)
	}

	// Failpoint: after_rename_tmp_to_main
	failpoint.Trigger("compaction_after_rename_tmp_to_main")

	// Remove bak
	_ = os.Remove(bakPath)

	// Failpoint: after_remove_bak
	failpoint.Trigger("compaction_after_remove_bak")

	// Swap succeeded
	cleanupNeeded = false

	// 8. Reopen new database file
	newFile, err := os.OpenFile(dbPath, os.O_RDWR, 0666)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reopen compacted database file: %w", err)
	}

	return &CompactResult{
		NewIndex:  newIdx,
		NewOffset: currentOffset,
		NextTxnID: nextTxnID,
	}, newFile, nil
}
