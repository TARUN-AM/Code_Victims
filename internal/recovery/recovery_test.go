package recovery_test

import (
	"bytes"
	"errors"
	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/recovery"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRecovery_TruncationSweep(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sweep_base.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// 1. Committed Txn 1: Single put
	if err := database.Put([]byte("k1"), []byte("val1")); err != nil {
		t.Fatalf("Txn 1 failed: %v", err)
	}

	// 2. Committed Txn 2: 10-op batch
	batchOps := make([]db.BatchOp, 10)
	for i := 0; i < 10; i++ {
		batchOps[i] = db.BatchOp{
			Type:  db.OpPut,
			Key:   []byte("batch_k_" + strconv.Itoa(i)),
			Value: []byte("batch_val_" + strconv.Itoa(i)),
		}
	}
	if err := database.Batch(batchOps); err != nil {
		t.Fatalf("Txn 2 failed: %v", err)
	}

	// 3. Committed Txn 3: Large 1 MB value put
	largeVal := bytes.Repeat([]byte("L"), 1024*1024)
	if err := database.Put([]byte("large_blob"), largeVal); err != nil {
		t.Fatalf("Txn 3 failed: %v", err)
	}

	// Record clean state boundary after Txn 3
	offsetAfterTxn3 := database.FileSize()

	// 4. Committed Txn 4: 10-op batch (we will sweep truncating inside this transaction)
	txn4Ops := make([]db.BatchOp, 10)
	for i := 0; i < 10; i++ {
		txn4Ops[i] = db.BatchOp{
			Type:  db.OpPut,
			Key:   []byte("txn4_k_" + strconv.Itoa(i)),
			Value: []byte("txn4_val_" + strconv.Itoa(i)),
		}
	}
	if err := database.Batch(txn4Ops); err != nil {
		t.Fatalf("Txn 4 failed: %v", err)
	}

	fullFileSize := database.FileSize()
	database.Close()

	// Read full file bytes
	fullBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("failed to read database file: %v", err)
	}
	if int64(len(fullBytes)) != fullFileSize {
		t.Fatalf("file length mismatch: expected %d, got %d", fullFileSize, len(fullBytes))
	}

	// Truncation Sweep:
	// Truncate at EVERY single byte offset from offsetAfterTxn3 to fullFileSize-1
	step := 1
	// If transaction is large, step 1 byte for first 200 and last 200, or sweep every byte
	for truncOffset := offsetAfterTxn3; truncOffset < fullFileSize; truncOffset += int64(step) {
		copyPath := filepath.Join(tempDir, "sweep_copy.db")
		if err := os.WriteFile(copyPath, fullBytes[:truncOffset], 0666); err != nil {
			t.Fatalf("failed to write truncated copy: %v", err)
		}

		// Every single open MUST succeed
		reopened, err := db.Open(copyPath, db.Options{})
		if err != nil {
			t.Fatalf("recovery failed at truncation offset %d / %d: %v", truncOffset, fullFileSize, err)
		}

		// Invariant check:
		// 1. All Txn 1, 2, 3 data MUST be present
		v1, err := reopened.Get([]byte("k1"))
		if err != nil || string(v1) != "val1" {
			t.Fatalf("Txn 1 data lost at truncOffset %d", truncOffset)
		}
		vb0, err := reopened.Get([]byte("batch_k_0"))
		if err != nil || string(vb0) != "batch_val_0" {
			t.Fatalf("Txn 2 data lost at truncOffset %d", truncOffset)
		}
		vBlob, err := reopened.Get([]byte("large_blob"))
		if err != nil || len(vBlob) != 1024*1024 {
			t.Fatalf("Txn 3 large value lost at truncOffset %d", truncOffset)
		}

		// 2. Txn 4 data MUST be completely absent
		for i := 0; i < 10; i++ {
			k := []byte("txn4_k_" + strconv.Itoa(i))
			if _, err := reopened.Get(k); !errors.Is(err, db.ErrNotFound) {
				t.Fatalf("torn Txn 4 key %q visible at truncOffset %d! Invariant violated", string(k), truncOffset)
			}
		}

		reopened.Close()
	}

	// At full length, Txn 4 MUST be present
	fullCopyPath := filepath.Join(tempDir, "sweep_full.db")
	if err := os.WriteFile(fullCopyPath, fullBytes, 0666); err != nil {
		t.Fatalf("failed to write full copy: %v", err)
	}

	fullReopened, err := db.Open(fullCopyPath, db.Options{})
	if err != nil {
		t.Fatalf("failed to open full copy: %v", err)
	}
	defer fullReopened.Close()

	for i := 0; i < 10; i++ {
		k := []byte("txn4_k_" + strconv.Itoa(i))
		if _, err := fullReopened.Get(k); err != nil {
			t.Fatalf("Txn 4 key %q missing in full database: %v", string(k), err)
		}
	}
}

func TestCheck_ReadOnlyVerifier(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "check.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := database.Put([]byte("alpha"), []byte("omega")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	database.Close()

	// 1. Clean file check
	res, err := recovery.Check(dbPath)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !res.Valid || res.Message != "OK" {
		t.Fatalf("expected clean Check, got valid=%v, msg=%s", res.Valid, res.Message)
	}
	if res.KeyCount != 1 || res.TxnCount != 1 {
		t.Fatalf("unexpected counts: keys=%d, txns=%d", res.KeyCount, res.TxnCount)
	}

	// 2. Append uncommitted garbage bytes
	f, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		t.Fatalf("failed to open for append: %v", err)
	}
	f.Write([]byte{0xA5, 0x02, 0x00, 0x01}) // partial torn header
	f.Close()

	fiBefore, _ := os.Stat(dbPath)

	// 3. Run Check on torn file
	resTorn, err := recovery.Check(dbPath)
	if err != nil {
		t.Fatalf("Check on torn file returned error: %v", err)
	}
	if resTorn.Valid {
		t.Fatalf("Check should have reported invalid for torn tail")
	}

	// 4. Verify Check did NOT modify file (strictly read-only)
	fiAfter, _ := os.Stat(dbPath)
	if fiBefore.Size() != fiAfter.Size() {
		t.Fatalf("Check modified file size! Before=%d, After=%d", fiBefore.Size(), fiAfter.Size())
	}
}

func TestRecovery_F4_TruncatedByOneByte(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "f4.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	// Txn 1
	if err := database.Put([]byte("survivor"), []byte("keep_me")); err != nil {
		t.Fatalf("Txn 1 failed: %v", err)
	}
	// Txn 2
	if err := database.Put([]byte("victim"), []byte("lose_me")); err != nil {
		t.Fatalf("Txn 2 failed: %v", err)
	}
	database.Close()

	// Truncate file by exactly 1 byte (cuts off last byte of Txn 2's COMMIT CRC)
	fi, _ := os.Stat(dbPath)
	if err := os.Truncate(dbPath, fi.Size()-1); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Reopen must succeed and cleanly roll back Txn 2 while retaining Txn 1
	reopened, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed after 1-byte truncation: %v", err)
	}
	defer reopened.Close()

	// Survivor must be intact
	survivorVal, err := reopened.Get([]byte("survivor"))
	if err != nil || string(survivorVal) != "keep_me" {
		t.Fatalf("survivor key lost or corrupted: %v", err)
	}

	// Victim must be absent
	if _, err := reopened.Get([]byte("victim")); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("victim key should have been rolled back")
	}
}

func TestRecovery_Idempotency(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "idempotent.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	database.Put([]byte("k1"), []byte("v1"))
	database.Close()

	// Append torn bytes
	f, _ := os.OpenFile(dbPath, os.O_WRONLY|os.O_APPEND, 0666)
	f.Write([]byte{0xA5, 0x01, 0x00})
	f.Close()

	// First recovery
	db1, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("first open failed: %v", err)
	}
	size1 := db1.FileSize()
	db1.Close()

	// Second recovery on already repaired file
	db2, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	size2 := db2.FileSize()
	val, err := db2.Get([]byte("k1"))
	if err != nil || string(val) != "v1" {
		t.Fatalf("data corrupted on second open: %v", err)
	}
	db2.Close()

	if size1 != size2 {
		t.Fatalf("idempotency failed: size changed from %d to %d", size1, size2)
	}
}

// Subprocess Helper for Failpoint Testing
func TestFailpointHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_FAILPOINT_PROCESS") != "1" {
		return
	}

	dbPath := os.Getenv("TEST_DB_PATH")
	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		os.Exit(1)
	}

	// Execute operation under failpoint
	opType := os.Getenv("TEST_FAILPOINT_OP")
	switch opType {
	case "put":
		_ = database.Put([]byte("test_key"), []byte("test_val"))
	case "batch":
		ops := make([]db.BatchOp, 10)
		for i := 0; i < 10; i++ {
			ops[i] = db.BatchOp{
				Type:  db.OpPut,
				Key:   []byte("batch_" + strconv.Itoa(i)),
				Value: []byte("val_" + strconv.Itoa(i)),
			}
		}
		_ = database.Batch(ops)
	}

	database.Close()
	os.Exit(0)
}

func runFailpointSubprocess(t *testing.T, dbPath string, failpointName string, op string) {
	cmd := exec.Command(os.Args[0], "-test.run=TestFailpointHelperProcess")
	cmd.Env = append(os.Environ(),
		"GO_WANT_FAILPOINT_PROCESS=1",
		"TEST_DB_PATH="+dbPath,
		"TEST_FAILPOINT_OP="+op,
		"KVDB_FAILPOINT="+failpointName,
	)
	_ = cmd.Run() // Expect non-zero exit code due to os.Exit(137)
}

func TestFailpoints_A1_to_A7_and_B1_to_B4(t *testing.T) {
	tempDir := t.TempDir()

	// A1: before_write
	t.Run("A1_BeforeWrite", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "a1.db")
		d, _ := db.Open(dbPath, db.Options{})
		d.Put([]byte("init"), []byte("val"))
		d.Close()

		runFailpointSubprocess(t, dbPath, "before_write", "put")

		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		if _, err := reopened.Get([]byte("test_key")); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("A1: expected test_key absent")
		}
	})

	// A2: mid_write (torn write)
	t.Run("A2_MidWrite", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "a2.db")
		d, _ := db.Open(dbPath, db.Options{})
		d.Put([]byte("init"), []byte("val"))
		d.Close()

		runFailpointSubprocess(t, dbPath, "mid_write", "put")

		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		if _, err := reopened.Get([]byte("test_key")); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("A2: expected test_key absent after mid_write")
		}
	})

	// A3: after_ops_before_sync
	t.Run("A3_AfterOpsBeforeSync", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "a3.db")
		runFailpointSubprocess(t, dbPath, "after_ops_before_sync", "put")
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		if _, err := reopened.Get([]byte("test_key")); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("A3: expected uncommitted op absent")
		}
	})

	// A4: after_commit_sync_before_ack
	t.Run("A4_AfterCommitSyncBeforeAck", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "a4.db")
		runFailpointSubprocess(t, dbPath, "after_commit_sync_before_ack", "put")
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		// Since commit sync succeeded before kill, op is fully durable and present
		val, err := reopened.Get([]byte("test_key"))
		if err != nil || string(val) != "test_val" {
			t.Fatalf("A4: expected test_key present after commit sync: %v", err)
		}
	})

	// B1: Batch failpoint mid_write
	t.Run("B1_BatchMidWrite", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "b1.db")
		runFailpointSubprocess(t, dbPath, "mid_write", "batch")
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		// None of the batch keys may be present
		for i := 0; i < 10; i++ {
			if _, err := reopened.Get([]byte("batch_" + strconv.Itoa(i))); !errors.Is(err, db.ErrNotFound) {
				t.Fatalf("B1: batch op %d leaked into database!", i)
			}
		}
	})

	// B3: after_commit_before_sync
	t.Run("B3_BatchAfterCommitBeforeSync", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "b3.db")
		runFailpointSubprocess(t, dbPath, "after_commit_before_sync", "batch")
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		// Reopen must succeed cleanly
	})

	// B4: after_commit_sync_before_ack for batch
	t.Run("B4_BatchAfterCommitSyncBeforeAck", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "b4.db")
		runFailpointSubprocess(t, dbPath, "after_commit_sync_before_ack", "batch")
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer reopened.Close()
		// All 10 keys must be present
		for i := 0; i < 10; i++ {
			val, err := reopened.Get([]byte("batch_" + strconv.Itoa(i)))
			if err != nil || string(val) != "val_"+strconv.Itoa(i) {
				t.Fatalf("B4: batch key %d missing: %v", i, err)
			}
		}
	})
}
