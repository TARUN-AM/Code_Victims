package db

import (
	"bytes"
	"errors"
	"github.com/TARUN-AM/torq/internal/limits"
	"github.com/TARUN-AM/torq/internal/wal"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPutGetDeleteOverwrite(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	database, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// 1. Put key
	key := []byte("session:user_123")
	val1 := []byte("active_v1")
	if err := database.Put(key, val1); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// 2. Get key
	got, err := database.Get(key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !bytes.Equal(got, val1) {
		t.Fatalf("value mismatch: expected %q, got %q", string(val1), string(got))
	}

	// 3. Overwrite key with larger value
	val2 := []byte("active_v2_longer_content_payload")
	if err := database.Put(key, val2); err != nil {
		t.Fatalf("Overwrite failed: %v", err)
	}

	got2, err := database.Get(key)
	if err != nil {
		t.Fatalf("Get after overwrite failed: %v", err)
	}
	if !bytes.Equal(got2, val2) {
		t.Fatalf("overwritten value mismatch: expected %q, got %q", string(val2), string(got2))
	}

	// 4. Delete key
	if err := database.Delete(key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// 5. Get deleted key must return ErrNotFound
	_, err = database.Get(key)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for deleted key, got %v", err)
	}

	// 6. Delete missing key must not return an error
	missingKey := []byte("missing:key:999")
	if err := database.Delete(missingKey); err != nil {
		t.Fatalf("Delete missing key should not return error, got %v", err)
	}
}

func TestBatchOrderingSemantics(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "batch.db")

	database, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Case 1: Put X, then Del X in the same batch -> Key X must be ABSENT
	batch1 := []BatchOp{
		{Type: OpPut, Key: []byte("key_x"), Value: []byte("val_x1")},
		{Type: OpDelete, Key: []byte("key_x")},
	}
	if err := database.Batch(batch1); err != nil {
		t.Fatalf("Batch 1 failed: %v", err)
	}

	_, err = database.Get([]byte("key_x"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for key_x (Put then Del), got %v", err)
	}

	// Case 2: Del Y, then Put Y in the same batch -> Key Y must be PRESENT
	batch2 := []BatchOp{
		{Type: OpDelete, Key: []byte("key_y")},
		{Type: OpPut, Key: []byte("key_y"), Value: []byte("val_y_final")},
	}
	if err := database.Batch(batch2); err != nil {
		t.Fatalf("Batch 2 failed: %v", err)
	}

	gotY, err := database.Get([]byte("key_y"))
	if err != nil {
		t.Fatalf("Get key_y failed: %v", err)
	}
	if string(gotY) != "val_y_final" {
		t.Fatalf("expected 'val_y_final', got %q", string(gotY))
	}
}

func TestLimits(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "limits.db")

	database, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// 1. Empty key rejected before lock
	if err := database.Put([]byte{}, []byte("val")); !errors.Is(err, wal.ErrKeyEmpty) {
		t.Fatalf("expected ErrKeyEmpty, got %v", err)
	}

	// 2. Oversized key (> 1024)
	largeKey := make([]byte, wal.MaxKeyLength+1)
	if err := database.Put(largeKey, []byte("val")); !errors.Is(err, wal.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge, got %v", err)
	}

	// 3. Oversized value (> 1 MB)
	largeVal := make([]byte, wal.MaxValueLength+1)
	if err := database.Put([]byte("valid_key"), largeVal); !errors.Is(err, wal.ErrValueTooLarge) {
		t.Fatalf("expected ErrValueTooLarge, got %v", err)
	}

	// 4. Batch limits
	if err := database.Batch([]BatchOp{}); !errors.Is(err, wal.ErrBatchEmpty) {
		t.Fatalf("expected ErrBatchEmpty, got %v", err)
	}

	oversizedBatch := make([]BatchOp, wal.MaxBatchSize+1)
	for i := range oversizedBatch {
		oversizedBatch[i] = BatchOp{Type: OpPut, Key: []byte("k"), Value: []byte("v")}
	}
	if err := database.Batch(oversizedBatch); !errors.Is(err, wal.ErrBatchTooLarge) {
		t.Fatalf("expected ErrBatchTooLarge, got %v", err)
	}
}

func TestDiskFull_LeavesFileByteIdentical(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "diskfull.db")

	// Header: 16 B
	// Txn 1: 44 B overhead + 24 + 4 + 4 = 76 B
	// Total after Txn 1 = 16 + 76 = 92 bytes.
	// We set MaxBytes = 92 so Txn 1 fits, but Txn 2 is rejected.
	maxBytes := int64(16 + 76)

	database, err := Open(dbPath, Options{MaxBytes: maxBytes})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Txn 1: fits within maxBytes
	if err := database.Put([]byte("key1"), []byte("val1")); err != nil {
		t.Fatalf("first Put failed: %v", err)
	}

	// Snapshot exact file bytes after first write
	bytesBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if int64(len(bytesBefore)) != maxBytes {
		t.Fatalf("expected file size %d, got %d", maxBytes, len(bytesBefore))
	}

	// Txn 2: should be rejected by preflight size-cap
	err = database.Put([]byte("key2"), []byte("val2"))
	if !errors.Is(err, limits.ErrDiskFull) {
		t.Fatalf("expected ErrDiskFull, got %v", err)
	}

	// Verify file is strictly byte-identical
	bytesAfter, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("failed to read file after rejection: %v", err)
	}

	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatalf("file bytes changed after ErrDiskFull rejection! Expected byte-identical file")
	}

	// Confirm DB remains readable
	val, err := database.Get([]byte("key1"))
	if err != nil {
		t.Fatalf("Get after rejection failed: %v", err)
	}
	if string(val) != "val1" {
		t.Fatalf("corrupted read after rejection: expected 'val1', got %q", string(val))
	}
}

func TestBatchStreaming(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "streaming.db")

	database, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// 1,000-op batch
	count := 1000
	ops := make([]BatchOp, count)
	for i := 0; i < count; i++ {
		ops[i] = BatchOp{
			Type:  OpPut,
			Key:   []byte("k_" + strconv.Itoa(i)),
			Value: []byte("val_" + strconv.Itoa(i)),
		}
	}

	if err := database.Batch(ops); err != nil {
		t.Fatalf("large batch failed: %v", err)
	}

	if database.KeyCount() != count {
		t.Fatalf("expected %d keys, got %d", count, database.KeyCount())
	}

	// Spot check keys
	v, err := database.Get([]byte("k_500"))
	if err != nil {
		t.Fatalf("Get k_500 failed: %v", err)
	}
	if string(v) != "val_500" {
		t.Fatalf("expected 'val_500', got %q", string(v))
	}
}

func TestOpenRejectsSecondProcessStyleHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	first, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, Options{})
	if err == nil {
		_ = second.Close()
		t.Fatal("expected second open to fail while first database owns process lock")
	}
}

func TestBatchRejectsUnknownOperationWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-op.db")
	database, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	before := database.FileSize()
	err = database.Batch([]BatchOp{{Type: BatchOpType(99), Key: []byte("safe-key")}})
	if err == nil {
		t.Fatal("expected unknown operation type to be rejected")
	}
	if got := database.FileSize(); got != before {
		t.Fatalf("invalid batch changed file size: before=%d after=%d", before, got)
	}
}
