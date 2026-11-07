package db

import (
	"errors"
	"fmt"
	"github.com/TARUN-AM/torq/internal/compaction"
	"github.com/TARUN-AM/torq/internal/index"
	"github.com/TARUN-AM/torq/internal/limits"
	"github.com/TARUN-AM/torq/internal/recovery"
	"github.com/TARUN-AM/torq/internal/wal"
	"os"
	"sync"
)

// Sentinel Database Errors
var (
	ErrNotFound      = errors.New("key not found")
	ErrClosed        = errors.New("database is closed")
	ErrWriteDisabled = errors.New("database is write-disabled due to previous unrecoverable write error")
)

// BatchOpType identifies the operation within an atomic batch.
type BatchOpType int

const (
	OpPut    BatchOpType = 1
	OpDelete BatchOpType = 2
)

// BatchOp represents a single operation within a batch.
type BatchOp struct {
	Type  BatchOpType
	Key   []byte
	Value []byte // Ignored for OpDelete
}

// Options configures the database instance.
type Options struct {
	MaxBytes int64 // Maximum database file size in bytes (0 = unlimited)
}

// DB represents a TORQ database instance.
type DB struct {
	mu            sync.RWMutex
	file          *os.File
	writer        *wal.Writer
	index         *index.Index
	path          string
	maxBytes      int64
	nextTxnID     uint64
	isClosed      bool
	writeDisabled bool
	processLock   *os.File
}

// Open opens or creates a TORQ database file at the specified path.
func Open(path string, opts Options) (*DB, error) {
	// Lock a stable sidecar before inspecting or changing compaction artifacts.
	processLock, err := acquireProcessLock(path)
	if err != nil {
		return nil, err
	}
	lockOwned := false
	defer func() {
		if !lockOwned {
			_ = releaseProcessLock(processLock)
		}
	}()

	// Resolve crash residues only while this process owns the database.
	if err := compaction.ResolveStartupResolution(path); err != nil {
		return nil, fmt.Errorf("compaction startup resolution failed: %w", err)
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open database file %q: %w", path, err)
	}

	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to stat database file: %w", err)
	}

	db := &DB{
		file:        file,
		path:        path,
		maxBytes:    opts.MaxBytes,
		index:       index.New(),
		nextTxnID:   1,
		processLock: processLock,
	}

	if fi.Size() == 0 {
		// New database: initialize 16-byte file header
		headerBuf := make([]byte, wal.FileHeaderSize)
		if err := wal.EncodeFileHeader(headerBuf); err != nil {
			file.Close()
			return nil, err
		}
		if _, err := file.WriteAt(headerBuf, 0); err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to write header: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to sync header: %w", err)
		}
		db.writer = wal.NewWriter(file, wal.FileHeaderSize)
	} else {
		// Existing database: run full crash recovery scan and repair
		recResult, err := recovery.Recover(file)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("recovery failed: %w", err)
		}
		db.index = recResult.Index
		db.writer = wal.NewWriter(file, recResult.LastCommittedOffset)
		db.nextTxnID = recResult.NextTxnID
	}

	lockOwned = true
	return db, nil
}

// Put writes a single key-value pair as a durable transaction of one operation.
func (db *DB) Put(key, value []byte) error {
	// 1. Validate limits before acquiring lock
	if err := limits.ValidateKey(key); err != nil {
		return err
	}
	if err := limits.ValidateValue(value); err != nil {
		return err
	}

	ops := []BatchOp{
		{Type: OpPut, Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)},
	}
	return db.writeTransaction(ops)
}

// Get reads the value for a given key.
// The value is read on demand from disk via ReadAt under the read lock;
// values are never held in RAM.
func (db *DB) Get(key []byte) ([]byte, error) {
	if err := limits.ValidateKey(key); err != nil {
		return nil, err
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.isClosed {
		return nil, ErrClosed
	}

	entry, ok := db.index.Get(string(key))
	if !ok {
		return nil, ErrNotFound
	}

	valBuf := make([]byte, entry.ValueLength)
	if entry.ValueLength > 0 {
		n, err := db.file.ReadAt(valBuf, entry.ValueOffset)
		if err != nil {
			return nil, fmt.Errorf("failed to read value at offset %d: %w", entry.ValueOffset, err)
		}
		if n != int(entry.ValueLength) {
			return nil, fmt.Errorf("short read for value: expected %d, got %d", entry.ValueLength, n)
		}
	}

	return valBuf, nil
}

// Delete marks a key as deleted by appending a DELETE transaction record.
// Deleting a missing key is not an error; it is logged as an atomic transaction.
func (db *DB) Delete(key []byte) error {
	if err := limits.ValidateKey(key); err != nil {
		return err
	}

	ops := []BatchOp{
		{Type: OpDelete, Key: append([]byte(nil), key...)},
	}
	return db.writeTransaction(ops)
}

// Batch applies an atomic list of operations (1..10,000 ops) as a single transaction.
func (db *DB) Batch(ops []BatchOp) error {
	// 1. Validate batch constraints before taking lock
	if err := limits.ValidateBatchSize(len(ops)); err != nil {
		return err
	}
	// Copy caller-owned buffers so concurrent caller mutation cannot change
	// the bytes persisted or the index offsets while a batch is in progress.
	owned := make([]BatchOp, len(ops))
	for i, op := range ops {
		if err := limits.ValidateKey(op.Key); err != nil {
			return fmt.Errorf("batch op %d: %w", i, err)
		}
		switch op.Type {
		case OpPut:
			if err := limits.ValidateValue(op.Value); err != nil {
				return fmt.Errorf("batch op %d: %w", i, err)
			}
		case OpDelete:
			if len(op.Value) != 0 {
				return fmt.Errorf("batch op %d: DELETE must not contain a value", i)
			}
		default:
			return fmt.Errorf("batch op %d: unknown operation type %d", i, op.Type)
		}
		owned[i] = BatchOp{Type: op.Type, Key: append([]byte(nil), op.Key...), Value: append([]byte(nil), op.Value...)}
	}

	return db.writeTransaction(owned)
}

// writeTransaction implements the strict write protocol:
// validate -> lock -> size-cap preflight -> stream BEGIN + ops -> Sync -> COMMIT -> Sync -> update index -> unlock.
func (db *DB) writeTransaction(ops []BatchOp) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.isClosed {
		return ErrClosed
	}
	if db.writeDisabled {
		return ErrWriteDisabled
	}

	// 1. Preflight size-cap calculation (BEGIN + ops + COMMIT)
	totalTxnSize := limits.EncodedTxnOverhead
	for _, op := range ops {
		if op.Type == OpPut {
			totalTxnSize += limits.EncodedPutSize(len(op.Key), len(op.Value))
		} else {
			totalTxnSize += limits.EncodedDeleteSize(len(op.Key))
		}
	}

	currentOffset := db.writer.Offset()
	if err := limits.CheckFit(currentOffset, totalTxnSize, db.maxBytes); err != nil {
		return limits.ErrDiskFull
	}

	// Failpoint: before_write
	TriggerFailpoint("before_write")

	txnID := db.nextTxnID
	startOffset := currentOffset

	// Internal helper to perform rollback on error
	rollback := func(cause error) error {
		if truncErr := db.writer.Truncate(startOffset); truncErr != nil {
			db.writeDisabled = true
			return fmt.Errorf("write error (%v) AND rollback truncate failed: %w", cause, truncErr)
		}
		return cause
	}

	// 2. Stream BEGIN record
	beginRec := &wal.Record{
		Type:    wal.RecordTypeBegin,
		TxnID:   txnID,
		OpCount: uint32(len(ops)),
	}
	beginBuf, err := wal.EncodeRecord(beginRec)
	if err != nil {
		return err
	}
	if err := db.writer.WriteAll(beginBuf); err != nil {
		return rollback(err)
	}

	// 3. Stream operation records
	committedOps := make([]index.Op, len(ops))
	isMidWrite := (ActiveFailpoint() == "mid_write")

	for i, op := range ops {
		recOffset := db.writer.Offset()
		var rec *wal.Record
		var committedOp index.Op

		if op.Type == OpPut {
			rec = &wal.Record{
				Type:  wal.RecordTypePut,
				TxnID: txnID,
				Key:   op.Key,
				Value: op.Value,
			}
			committedOp = index.Op{
				IsDelete:    false,
				Key:         string(op.Key),
				ValueOffset: recOffset + int64(wal.RecordHeaderSize+6+len(op.Key)),
				ValueLength: uint32(len(op.Value)),
			}
		} else {
			rec = &wal.Record{
				Type:  wal.RecordTypeDelete,
				TxnID: txnID,
				Key:   op.Key,
			}
			committedOp = index.Op{
				IsDelete: true,
				Key:      string(op.Key),
			}
		}

		opBuf, err := wal.EncodeRecord(rec)
		if err != nil {
			return rollback(err)
		}

		if isMidWrite && i == 0 {
			// Write half bytes and trigger failpoint
			half := len(opBuf) / 2
			_ = db.writer.WritePartial(opBuf[:half], half)
			TriggerFailpoint("mid_write")
		}

		if err := db.writer.WriteAll(opBuf); err != nil {
			return rollback(err)
		}

		committedOps[i] = committedOp
	}

	// Failpoint: after_ops_before_sync
	TriggerFailpoint("after_ops_before_sync")

	// 4. Sync ops to disk (flush dirty pages)
	if err := db.writer.Sync(); err != nil {
		return rollback(fmt.Errorf("sync ops failed: %w", err))
	}

	// Failpoint: after_ops_sync_before_commit
	TriggerFailpoint("after_ops_sync_before_commit")

	// 5. Stream COMMIT record
	commitRec := &wal.Record{
		Type:    wal.RecordTypeCommit,
		TxnID:   txnID,
		OpCount: uint32(len(ops)),
	}
	commitBuf, err := wal.EncodeRecord(commitRec)
	if err != nil {
		return rollback(err)
	}
	if err := db.writer.WriteAll(commitBuf); err != nil {
		return rollback(err)
	}

	// Failpoint: after_commit_before_sync
	TriggerFailpoint("after_commit_before_sync")

	// 6. Sync COMMIT to disk (this completes the durable commit point)
	if err := db.writer.Sync(); err != nil {
		return rollback(fmt.Errorf("sync commit failed: %w", err))
	}

	// Failpoint: after_commit_sync_before_ack
	TriggerFailpoint("after_commit_sync_before_ack")

	// 7. Update in-memory index
	db.index.ApplyCommittedOps(committedOps)

	// 8. Monotonically increment nextTxnID
	db.nextTxnID++

	return nil
}

// Close gracefully closes the database.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.isClosed {
		return ErrClosed
	}

	// Failpoint: close_mid
	TriggerFailpoint("close_mid")

	db.isClosed = true
	var syncErr, closeErr error
	if db.file != nil {
		syncErr = db.file.Sync()
		closeErr = db.file.Close()
	}

	lockErr := releaseProcessLock(db.processLock)
	db.processLock = nil
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return lockErr
}

// KeyCount returns the number of live keys in the database.
func (db *DB) KeyCount() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.index.Len()
}

// FileSize returns the current byte length of the database file on disk.
func (db *DB) FileSize() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.writer == nil {
		return 0
	}
	return db.writer.Offset()
}

// AllKeys returns a slice of all live keys in the database.
func (db *DB) AllKeys() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.index.Keys()
}

// LastTxnID returns the most recently committed transaction ID.
func (db *DB) LastTxnID() uint64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.nextTxnID > 1 {
		return db.nextTxnID - 1
	}
	return 1
}

// Compact rebuilds the database file containing only live keys and latest values.
// Readers and writers are blocked during the swap under the exclusive write lock.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.isClosed {
		return ErrClosed
	}
	if db.writeDisabled {
		return ErrWriteDisabled
	}

	result, newFile, err := compaction.ExecuteCompaction(db.path, db.file, db.index, db.maxBytes)
	if err != nil {
		if db.file != nil {
			if _, statErr := db.file.Stat(); statErr != nil {
				if reopened, openErr := os.OpenFile(db.path, os.O_RDWR, 0666); openErr == nil {
					db.file = reopened
					if rec, recErr := recovery.Recover(reopened); recErr == nil {
						db.index = rec.Index
						db.writer = wal.NewWriter(reopened, rec.LastCommittedOffset)
						db.nextTxnID = rec.NextTxnID
					} else {
						_ = reopened.Close()
						db.writeDisabled = true
					}
				} else {
					db.writeDisabled = true
				}
			}
		}
		return fmt.Errorf("compaction failed: %w", err)
	}

	db.file = newFile
	db.index = result.NewIndex
	db.writer = wal.NewWriter(newFile, result.NewOffset)
	db.nextTxnID = result.NextTxnID

	return nil
}
