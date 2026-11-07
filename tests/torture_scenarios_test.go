package tests_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/recovery"
	"github.com/TARUN-AM/torq/internal/wal"
)

// Helper process for failpoint testing
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	dbPath := os.Getenv("TEST_DB_PATH")
	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		os.Exit(1)
	}
	_ = database.Close()
	os.Exit(0)
}

// Scenario: 1 MB maximum limit values
func TestLargeValues_1MB(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "large_val.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	val1MB := make([]byte, wal.MaxValueLength) // 1,048,576 bytes
	if _, err := rand.Read(val1MB); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}

	// 1. Put 1 MB blob
	key := []byte("blob_1mb_key")
	if err := database.Put(key, val1MB); err != nil {
		t.Fatalf("Put 1MB blob failed: %v", err)
	}

	// 2. Get 1 MB blob and verify byte-for-byte exact equality
	retrieved, err := database.Get(key)
	if err != nil {
		t.Fatalf("Get 1MB blob failed: %v", err)
	}
	if !bytes.Equal(retrieved, val1MB) {
		t.Fatalf("1MB blob byte mismatch! len(retrieved)=%d, len(expected)=%d", len(retrieved), len(val1MB))
	}

	// 3. Close and reopen to verify crash-recovery reads 1MB value correctly
	if err := database.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	reopened, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer reopened.Close()

	retrievedAfterRecovery, err := reopened.Get(key)
	if err != nil {
		t.Fatalf("Get 1MB blob after recovery failed: %v", err)
	}
	if !bytes.Equal(retrievedAfterRecovery, val1MB) {
		t.Fatalf("1MB blob byte mismatch after recovery!")
	}

	// Verify check passes
	res, err := recovery.Check(dbPath)
	if err != nil || !res.Valid {
		t.Fatalf("Check failed on 1MB database: %v", err)
	}
}

// Scenario: 10,000 operations in a single atomic batch
func TestLargeBatch_10000Ops(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "large_batch.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const batchSize = wal.MaxBatchSize // 10,000
	ops := make([]db.BatchOp, batchSize)

	for i := 0; i < batchSize; i++ {
		k := []byte(fmt.Sprintf("k_%05d", i))
		v := []byte(fmt.Sprintf("v_%05d_%d", i, i*7))
		ops[i] = db.BatchOp{
			Type:  db.OpPut,
			Key:   k,
			Value: v,
		}
	}

	// Execute 10,000-op atomic batch
	if err := database.Batch(ops); err != nil {
		t.Fatalf("10,000-op Batch failed: %v", err)
	}

	if database.KeyCount() != batchSize {
		t.Fatalf("expected KeyCount %d, got %d", batchSize, database.KeyCount())
	}

	// Verify first, middle, and last keys
	for _, idx := range []int{0, 4999, 9999} {
		k := []byte(fmt.Sprintf("k_%05d", idx))
		expectedV := []byte(fmt.Sprintf("v_%05d_%d", idx, idx*7))
		val, err := database.Get(k)
		if err != nil {
			t.Fatalf("Get key %s failed: %v", k, err)
		}
		if !bytes.Equal(val, expectedV) {
			t.Fatalf("Value mismatch for key %s", k)
		}
	}

	// Close and reopen to verify full recovery of 10,000 keys
	if err := database.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	reopened, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer reopened.Close()

	if reopened.KeyCount() != batchSize {
		t.Fatalf("reopened KeyCount mismatch: expected %d, got %d", batchSize, reopened.KeyCount())
	}
}

// Scenarios F1, F2, F3: Crashes during recovery must be completely idempotent
func TestCrashDuringRecovery_F1_to_F3(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "recovery_crash.db")

	// 1. Populate a database with 20 committed transactions
	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	for i := 0; i < 20; i++ {
		_ = database.Put([]byte("key_"+strconv.Itoa(i)), []byte("val_"+strconv.Itoa(i)))
	}
	_ = database.Close()

	// 2. Append an uncommitted transaction tail manually (simulating crash before commit)
	f, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	beginRec := &wal.Record{
		Type:    wal.RecordTypeBegin,
		TxnID:   21,
		OpCount: 1,
	}
	beginBuf, _ := wal.EncodeRecord(beginRec)
	_, _ = f.Write(beginBuf)
	putRec := &wal.Record{
		Type:  wal.RecordTypePut,
		TxnID: 21,
		Key:   []byte("uncommitted_key"),
		Value: []byte("uncommitted_val"),
	}
	putBuf, _ := wal.EncodeRecord(putRec)
	_, _ = f.Write(putBuf)
	_ = f.Sync()
	_ = f.Close()

	// Helper runner
	runWithFailpoint := func(fpName string) int {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"TEST_DB_PATH="+dbPath,
			"KVDB_FAILPOINT="+fpName,
		)
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode()
	}

	// F1: Crash mid-scan during recovery
	codeF1 := runWithFailpoint("recovery_mid_scan")
	if codeF1 != 137 {
		t.Fatalf("expected F1 exit 137, got %d", codeF1)
	}

	// F2: Crash before truncate during recovery
	codeF2 := runWithFailpoint("recovery_before_truncate")
	if codeF2 != 137 {
		t.Fatalf("expected F2 exit 137, got %d", codeF2)
	}

	// F3: Crash after truncate during recovery
	codeF3 := runWithFailpoint("recovery_after_truncate")
	if codeF3 != 137 {
		t.Fatalf("expected F3 exit 137, got %d", codeF3)
	}

	// 3. Finally run recovery with NO failpoint: must recover 100% cleanly
	finalDB, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Final Open after recovery crashes failed: %v", err)
	}
	defer finalDB.Close()

	// All 20 committed keys must be intact
	for i := 0; i < 20; i++ {
		val, err := finalDB.Get([]byte("key_" + strconv.Itoa(i)))
		if err != nil || string(val) != "val_"+strconv.Itoa(i) {
			t.Fatalf("committed key %d lost or corrupt after recovery crashes: %v", i, err)
		}
	}

	// Uncommitted key must be absent
	if _, err := finalDB.Get([]byte("uncommitted_key")); err != db.ErrNotFound {
		t.Fatalf("uncommitted key should be absent after recovery, got err=%v", err)
	}

	// Check must be OK
	res, err := recovery.Check(dbPath)
	if err != nil || !res.Valid {
		t.Fatalf("Check failed: %v, msg=%s", err, res.Message)
	}
}
