package recovery

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/TARUN-AM/torq/internal/failpoint"
	"github.com/TARUN-AM/torq/internal/index"
	"github.com/TARUN-AM/torq/internal/wal"
	"io"
	"os"
)

// Result holds the state recovered from a sequential WAL replay scan.
type Result struct {
	Index               *index.Index
	LastCommittedOffset int64
	NextTxnID           uint64
	CommittedTxnCount   int
	Repaired            bool
	RepairOffset        int64
}

// isInvalidFrameAtEOF reports whether a malformed frame at offset occupies
// exactly the remaining physical tail. It never treats middle-file corruption
// as a recoverable interrupted append.
func isInvalidFrameAtEOF(file *os.File, offset, fileSize int64) bool {
	var header [wal.RecordHeaderSize]byte
	if _, err := file.ReadAt(header[:], offset); err != nil {
		return false
	}
	bodyLen := int64(binary.LittleEndian.Uint32(header[10:14]))
	if bodyLen < 0 || bodyLen > int64(6+wal.MaxKeyLength+wal.MaxValueLength) {
		return false
	}
	return offset+int64(wal.RecordEnvelopeSize)+bodyLen == fileSize
}

// CheckResult holds the status returned by the read-only verification scan.
type CheckResult struct {
	Valid          bool
	KeyCount       int
	TxnCount       int
	FileSize       int64
	CommittedBytes int64
	Message        string
}

// Recover performs full recovery on an opened database file:
//  1. Validates the 16-byte header.
//  2. Scans sequential records with a reusable 1 MB buffered reader.
//  3. Applies transactions to the index ONLY after their COMMIT record validates.
//  4. If an incomplete record, invalid CRC, or uncommitted transaction tail is encountered,
//     it truncates the file back to LastCommittedOffset, calls file.Sync(), and sets nextTxnID.
func Recover(file *os.File) (*Result, error) {
	fi, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file for recovery: %w", err)
	}

	fileSize := fi.Size()
	if fileSize < wal.FileHeaderSize {
		return nil, fmt.Errorf("%w: file size %d smaller than header", wal.ErrIncompleteRecord, fileSize)
	}

	// 1. Validate file header
	headerBuf := make([]byte, wal.FileHeaderSize)
	if _, err := file.ReadAt(headerBuf, 0); err != nil {
		return nil, fmt.Errorf("failed to read file header: %w", err)
	}
	if _, err := wal.DecodeFileHeader(headerBuf); err != nil {
		return nil, err
	}

	// 2. Sequential Scan
	if _, err := file.Seek(wal.FileHeaderSize, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to start of WAL failed: %w", err)
	}

	reader := wal.NewReader(file, wal.FileHeaderSize)
	estimatedKeys := int(fileSize / 40)
	if estimatedKeys < 16 {
		estimatedKeys = 16
	}
	idx := index.NewWithCapacity(estimatedKeys)

	lastCommittedOffset := int64(wal.FileHeaderSize)
	var maxCommittedTxnID uint64 = 0
	committedTxnCount := 0

	inTxn := false
	var currentTxnID uint64 = 0
	var expectedOpCount uint32 = 0
	var actualOpCount uint32 = 0
	var inFlightOps []index.Op

	recordCount := 0

	for {
		// Failpoint: recovery_mid_scan
		recordCount++
		if recordCount == 5 {
			failpoint.Trigger("recovery_mid_scan")
		}

		rec, loc, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean EOF
				if inTxn {
					// File ended cleanly at byte boundary, but inside an uncommitted transaction
					// Must truncate back to lastCommittedOffset
					break
				}
				// Clean end of file outside any transaction
				break
			}
			// A physically incomplete final record is consistent with process
			// termination during append and may be truncated. A complete record
			// with a bad marker/checksum is corruption: fail closed rather than
			// silently discarding potentially later committed transactions.
			if errors.Is(err, wal.ErrIncompleteRecord) {
				break
			}
			// A checksum/marker failure confined to the physical tail can be
			// the result of process termination during the final append. Only
			// tolerate it when the malformed frame demonstrably ends at EOF;
			// corruption with any later bytes is never silently truncated.
			if errors.Is(err, wal.ErrInvalidRecord) && isInvalidFrameAtEOF(file, reader.Offset(), fileSize) {
				break
			}
			return nil, fmt.Errorf("WAL corruption at offset %d: %w", reader.Offset(), err)
		}

		switch rec.Type {
		case wal.RecordTypeBegin:
			if inTxn {
				return nil, fmt.Errorf("WAL corruption at offset %d: nested BEGIN", loc.Offset)
			}
			inTxn = true
			currentTxnID = rec.TxnID
			expectedOpCount = rec.OpCount
			actualOpCount = 0
			if cap(inFlightOps) < int(rec.OpCount) {
				inFlightOps = make([]index.Op, 0, rec.OpCount)
			} else {
				inFlightOps = inFlightOps[:0]
			}

		case wal.RecordTypePut:
			if !inTxn || rec.TxnID != currentTxnID {
				return nil, fmt.Errorf("WAL corruption at offset %d: PUT outside or mismatched transaction", loc.Offset)
			}
			actualOpCount++
			if actualOpCount > expectedOpCount {
				return nil, fmt.Errorf("WAL corruption at offset %d: transaction contains more operations than declared", loc.Offset)
			}
			inFlightOps = append(inFlightOps, index.Op{
				IsDelete:    false,
				Key:         string(rec.Key),
				ValueOffset: loc.ValueOffset,
				ValueLength: loc.ValueLength,
			})

		case wal.RecordTypeDelete:
			if !inTxn || rec.TxnID != currentTxnID {
				return nil, fmt.Errorf("WAL corruption at offset %d: DELETE outside or mismatched transaction", loc.Offset)
			}
			actualOpCount++
			if actualOpCount > expectedOpCount {
				return nil, fmt.Errorf("WAL corruption at offset %d: transaction contains more operations than declared", loc.Offset)
			}
			inFlightOps = append(inFlightOps, index.Op{
				IsDelete: true,
				Key:      string(rec.Key),
			})

		case wal.RecordTypeCommit:
			if !inTxn || rec.TxnID != currentTxnID || rec.OpCount != expectedOpCount || actualOpCount != expectedOpCount {
				return nil, fmt.Errorf("WAL corruption at offset %d: invalid COMMIT framing", loc.Offset)
			}

			// Valid COMMIT: atomically apply in-flight ops to index
			idx.ApplyCommittedOps(inFlightOps)
			lastCommittedOffset = loc.Offset + int64(loc.TotalLength)
			if currentTxnID > maxCommittedTxnID {
				maxCommittedTxnID = currentTxnID
			}
			committedTxnCount++
			inTxn = false
		}
	}

	repaired := false
	if fileSize > lastCommittedOffset {
		// Failpoint: recovery_before_truncate
		failpoint.Trigger("recovery_before_truncate")

		// Truncate uncommitted / torn tail
		if err := file.Truncate(lastCommittedOffset); err != nil {
			return nil, fmt.Errorf("recovery failed to truncate tail to %d: %w", lastCommittedOffset, err)
		}
		if err := file.Sync(); err != nil {
			return nil, fmt.Errorf("recovery failed to sync truncated file: %w", err)
		}

		// Failpoint: recovery_after_truncate
		failpoint.Trigger("recovery_after_truncate")

		repaired = true
	}

	// Seek file pointer to the valid end of file
	if _, err := file.Seek(lastCommittedOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek to committed offset: %w", err)
	}

	return &Result{
		Index:               idx,
		LastCommittedOffset: lastCommittedOffset,
		NextTxnID:           maxCommittedTxnID + 1,
		CommittedTxnCount:   committedTxnCount,
		Repaired:            repaired,
		RepairOffset:        lastCommittedOffset,
	}, nil
}

// Check performs a strictly READ-ONLY verification of the database file.
// It never truncates, modifies, or writes to the file.
func Check(path string) (*CheckResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %q: %w", path, err)
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := fi.Size()

	if fileSize < wal.FileHeaderSize {
		return &CheckResult{
			Valid:    false,
			FileSize: fileSize,
			Message:  fmt.Sprintf("file too small (%d < %d bytes header)", fileSize, wal.FileHeaderSize),
		}, nil
	}

	// Validate header
	headerBuf := make([]byte, wal.FileHeaderSize)
	if _, err := file.ReadAt(headerBuf, 0); err != nil {
		return nil, err
	}
	if _, err := wal.DecodeFileHeader(headerBuf); err != nil {
		return &CheckResult{
			Valid:    false,
			FileSize: fileSize,
			Message:  fmt.Sprintf("corrupt file header: %v", err),
		}, nil
	}

	if _, err := file.Seek(wal.FileHeaderSize, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to start of WAL failed: %w", err)
	}

	reader := wal.NewReader(bufio.NewReader(file), wal.FileHeaderSize)
	idx := index.New()

	lastCommittedOffset := int64(wal.FileHeaderSize)
	committedTxns := 0
	lastCommittedTxnID := uint64(0)
	inTxn := false
	var currentTxnID uint64 = 0
	var expectedOpCount uint32 = 0
	var actualOpCount uint32 = 0
	var inFlightOps []index.Op

	for {
		rec, loc, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if inTxn {
					return &CheckResult{
						Valid:          false,
						FileSize:       fileSize,
						CommittedBytes: lastCommittedOffset,
						KeyCount:       idx.Len(),
						TxnCount:       committedTxns,
						Message:        fmt.Sprintf("uncommitted transaction %d at EOF (committed up to offset %d)", currentTxnID, lastCommittedOffset),
					}, nil
				}
				break
			}
			return &CheckResult{
				Valid:          false,
				FileSize:       fileSize,
				CommittedBytes: lastCommittedOffset,
				KeyCount:       idx.Len(),
				TxnCount:       committedTxns,
				Message:        fmt.Sprintf("scan stopped at offset %d: %v", reader.Offset(), err),
			}, nil
		}

		switch rec.Type {
		case wal.RecordTypeBegin:
			if rec.OpCount == 0 || rec.OpCount > wal.MaxBatchSize || rec.TxnID == 0 || rec.TxnID <= lastCommittedTxnID {
				return &CheckResult{Valid: false, Message: fmt.Sprintf("invalid BEGIN metadata at offset %d", loc.Offset)}, nil
			}
			if inTxn {
				return &CheckResult{
					Valid:   false,
					Message: fmt.Sprintf("nested BEGIN at offset %d", loc.Offset),
				}, nil
			}
			inTxn = true
			currentTxnID = rec.TxnID
			expectedOpCount = rec.OpCount
			actualOpCount = 0
			inFlightOps = make([]index.Op, 0, rec.OpCount)

		case wal.RecordTypePut:
			if !inTxn || rec.TxnID != currentTxnID {
				return &CheckResult{
					Valid:   false,
					Message: fmt.Sprintf("orphaned PUT at offset %d", loc.Offset),
				}, nil
			}
			actualOpCount++
			if actualOpCount > expectedOpCount {
				return &CheckResult{Valid: false, Message: fmt.Sprintf("operation count exceeds BEGIN at offset %d", loc.Offset)}, nil
			}
			inFlightOps = append(inFlightOps, index.Op{
				IsDelete:    false,
				Key:         string(rec.Key),
				ValueOffset: loc.ValueOffset,
				ValueLength: loc.ValueLength,
			})

		case wal.RecordTypeDelete:
			if !inTxn || rec.TxnID != currentTxnID {
				return &CheckResult{
					Valid:   false,
					Message: fmt.Sprintf("orphaned DELETE at offset %d", loc.Offset),
				}, nil
			}
			actualOpCount++
			if actualOpCount > expectedOpCount {
				return &CheckResult{Valid: false, Message: fmt.Sprintf("operation count exceeds BEGIN at offset %d", loc.Offset)}, nil
			}
			inFlightOps = append(inFlightOps, index.Op{
				IsDelete: true,
				Key:      string(rec.Key),
			})

		case wal.RecordTypeCommit:
			if !inTxn || rec.TxnID != currentTxnID || rec.OpCount != expectedOpCount || actualOpCount != expectedOpCount {
				return &CheckResult{
					Valid:   false,
					Message: fmt.Sprintf("mismatched COMMIT at offset %d", loc.Offset),
				}, nil
			}
			idx.ApplyCommittedOps(inFlightOps)
			lastCommittedOffset = loc.Offset + int64(loc.TotalLength)
			committedTxns++
			lastCommittedTxnID = currentTxnID
			inTxn = false
			inFlightOps = nil
		}
	}

	if fileSize != lastCommittedOffset {
		return &CheckResult{
			Valid:          false,
			FileSize:       fileSize,
			CommittedBytes: lastCommittedOffset,
			KeyCount:       idx.Len(),
			TxnCount:       committedTxns,
			Message:        fmt.Sprintf("file has %d uncommitted trailing bytes beyond offset %d", fileSize-lastCommittedOffset, lastCommittedOffset),
		}, nil
	}

	return &CheckResult{
		Valid:          true,
		FileSize:       fileSize,
		CommittedBytes: lastCommittedOffset,
		KeyCount:       idx.Len(),
		TxnCount:       committedTxns,
		Message:        "OK",
	}, nil
}
