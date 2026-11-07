package tests_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/limits"
	"github.com/TARUN-AM/torq/internal/recovery"
)

// Scenario E1: Disk full on single write.
// Preflight rejection leaves file byte-for-byte identical (no partial bytes appended).
func TestDiskFull_E1_SinglePutByteIdentical(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "e1_diskfull.db")

	// Limit to 200 bytes
	database, err := db.Open(dbPath, db.Options{MaxBytes: 200})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Initial write that fits
	if err := database.Put([]byte("key1"), []byte("val1")); err != nil {
		t.Fatalf("initial Put failed: %v", err)
	}

	preBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	preSize := len(preBytes)

	// Attempt a Put that would exceed 200 bytes
	largeVal := make([]byte, 150)
	err = database.Put([]byte("key_overflow"), largeVal)
	if !errors.Is(err, limits.ErrDiskFull) {
		t.Fatalf("expected ErrDiskFull, got %v", err)
	}

	// Read file bytes from disk: must be 100% byte-for-byte identical
	postBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if len(postBytes) != preSize {
		t.Fatalf("file size changed after ErrDiskFull: was %d, now %d", preSize, len(postBytes))
	}
	if !bytes.Equal(postBytes, preBytes) {
		t.Fatalf("file bytes mutated after preflight ErrDiskFull!")
	}

	// Verify key1 is intact and key_overflow is absent
	v, err := database.Get([]byte("key1"))
	if err != nil || string(v) != "val1" {
		t.Fatalf("key1 corrupted or lost: %v", err)
	}
	if _, err := database.Get([]byte("key_overflow")); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for overflow key, got %v", err)
	}
}

// Scenario E2: Disk full during batch write.
// Preflight calculation rejects the entire batch before writing a single byte.
func TestDiskFull_E2_BatchRejectionByteIdentical(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "e2_diskfull.db")

	database, err := db.Open(dbPath, db.Options{MaxBytes: 250})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Seed keyA
	if err := database.Put([]byte("keyA"), []byte("valA")); err != nil {
		t.Fatalf("Put keyA failed: %v", err)
	}

	preBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// Create a batch of 5 ops that in total exceeds capacity
	batchOps := []db.BatchOp{
		{Type: db.OpPut, Key: []byte("b_k1"), Value: []byte("val1")},
		{Type: db.OpPut, Key: []byte("b_k2"), Value: make([]byte, 100)},
		{Type: db.OpPut, Key: []byte("b_k3"), Value: make([]byte, 100)},
	}

	err = database.Batch(batchOps)
	if !errors.Is(err, limits.ErrDiskFull) {
		t.Fatalf("expected ErrDiskFull on batch, got %v", err)
	}

	// File bytes must remain completely unchanged
	postBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(postBytes, preBytes) {
		t.Fatalf("file bytes modified by rejected batch!")
	}

	// None of the batch keys must be present
	for _, op := range batchOps {
		if _, err := database.Get(op.Key); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("batch key %s should not exist, got err=%v", op.Key, err)
		}
	}
}

// Scenario E3: Read-only operations succeed when disk is full.
func TestDiskFull_E3_ReadOnlyOperationsSucceed(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "e3_diskfull.db")

	// Pre-fill a database with 3 keys
	dInit, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_ = dInit.Put([]byte("user:1"), []byte("Alice"))
	_ = dInit.Put([]byte("user:2"), []byte("Bob"))
	_ = dInit.Put([]byte("user:3"), []byte("Charlie"))
	fullSize := dInit.FileSize()
	_ = dInit.Close()

	// Reopen with MaxBytes equal to current FileSize (database is 100% full)
	database, err := db.Open(dbPath, db.Options{MaxBytes: fullSize})
	if err != nil {
		t.Fatalf("Open failed at full capacity: %v", err)
	}
	defer database.Close()

	// Any new Put must fail with ErrDiskFull
	if err := database.Put([]byte("user:4"), []byte("Dave")); !errors.Is(err, limits.ErrDiskFull) {
		t.Fatalf("expected ErrDiskFull, got %v", err)
	}

	// All Read operations MUST succeed
	v1, err := database.Get([]byte("user:1"))
	if err != nil || string(v1) != "Alice" {
		t.Fatalf("Get user:1 failed on full database: %v", err)
	}
	v2, err := database.Get([]byte("user:2"))
	if err != nil || string(v2) != "Bob" {
		t.Fatalf("Get user:2 failed on full database: %v", err)
	}

	// KeyCount and AllKeys must succeed
	if database.KeyCount() != 3 {
		t.Fatalf("expected 3 keys, got %d", database.KeyCount())
	}
	allKeys := database.AllKeys()
	if len(allKeys) != 3 {
		t.Fatalf("expected 3 keys from AllKeys, got %d", len(allKeys))
	}

	// Check verifier must report OK
	res, err := recovery.Check(dbPath)
	if err != nil || !res.Valid {
		t.Fatalf("Check failed on full database: %v", err)
	}
}

// Scenario E4: Recovery succeeds on disk-full database without requiring spare disk space.
func TestDiskFull_E4_RecoverySucceedsOnFullDatabase(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "e4_diskfull.db")

	dInit, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	for i := 0; i < 15; i++ {
		_ = dInit.Put([]byte(fmt.Sprintf("k_%02d", i)), []byte(fmt.Sprintf("val_%02d", i)))
	}
	exactSize := dInit.FileSize()
	_ = dInit.Close()

	// Open at exact size limit
	dFull, err := db.Open(dbPath, db.Options{MaxBytes: exactSize})
	if err != nil {
		t.Fatalf("Open failed at exact size limit: %v", err)
	}

	if dFull.KeyCount() != 15 {
		t.Fatalf("expected 15 recovered keys on full db, got %d", dFull.KeyCount())
	}

	for i := 0; i < 15; i++ {
		v, err := dFull.Get([]byte(fmt.Sprintf("k_%02d", i)))
		if err != nil || string(v) != fmt.Sprintf("val_%02d", i) {
			t.Fatalf("data mismatch on full db recovery: %v", err)
		}
	}
	_ = dFull.Close()
}

// Scenario E5: Compaction reclaims dead space and restores write capability on a full database.
func TestDiskFull_E5_CompactionReclaimsSpaceAndRestoresWrites(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "e5_diskfull.db")

	database, err := db.Open(dbPath, db.Options{MaxBytes: 5000})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// 1. Write 5 keys
	for i := 0; i < 5; i++ {
		_ = database.Put([]byte(fmt.Sprintf("key_%d", i)), []byte("initial_value"))
	}

	// 2. Overwrite those 5 keys repeatedly until the file fills up to the 5000 byte limit
	var filled bool
	for round := 0; round < 100; round++ {
		err := database.Put([]byte(fmt.Sprintf("key_%d", round%5)), []byte(fmt.Sprintf("overwritten_round_%d", round)))
		if errors.Is(err, limits.ErrDiskFull) {
			filled = true
			break
		}
	}
	if !filled {
		t.Fatalf("failed to fill database to 5000 bytes limit")
	}

	// Verify that new writes are blocked
	if err := database.Put([]byte("new_key"), []byte("blocked")); !errors.Is(err, limits.ErrDiskFull) {
		t.Fatalf("expected ErrDiskFull before compaction, got %v", err)
	}

	uncompactedSize := database.FileSize()

	// 3. Compact the database: removes obsolete overwrites
	if err := database.Compact(); err != nil {
		t.Fatalf("Compact failed on full database: %v", err)
	}

	compactedSize := database.FileSize()
	if compactedSize >= uncompactedSize {
		t.Fatalf("compactedSize (%d) did not shrink from %d", compactedSize, uncompactedSize)
	}

	// 4. Writing a new key now MUST succeed because dead space was reclaimed!
	if err := database.Put([]byte("new_key"), []byte("write_restored_successfully")); err != nil {
		t.Fatalf("Put failed after compaction reclaimed space: %v", err)
	}

	// Verify new key is readable
	val, err := database.Get([]byte("new_key"))
	if err != nil || string(val) != "write_restored_successfully" {
		t.Fatalf("Get on newly written key failed: %v", err)
	}
}
