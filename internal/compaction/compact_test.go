package compaction_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/recovery"
)

// Helper process for compaction failpoint testing
func TestHelperCompactionProcess(t *testing.T) {
	if os.Getenv("GO_WANT_COMPACT_HELPER") != "1" {
		return
	}
	dbPath := os.Getenv("TEST_DB_PATH")
	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		os.Exit(1)
	}
	err = database.Compact()
	if err != nil {
		os.Exit(1)
	}
	_ = database.Close()
	os.Exit(0)
}

func TestCompaction_ReducesSizeAndPreservesKeys(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "compact_basic.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Write 100 keys
	for i := 0; i < 100; i++ {
		k := []byte(fmt.Sprintf("key_%03d", i))
		v := []byte(fmt.Sprintf("val_init_%03d", i))
		if err := database.Put(k, v); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}

	// Overwrite all 100 keys 4 more times (generating 400 obsolete PUT records)
	for round := 1; round <= 4; round++ {
		for i := 0; i < 100; i++ {
			k := []byte(fmt.Sprintf("key_%03d", i))
			v := []byte(fmt.Sprintf("val_round_%d_%03d", round, i))
			if err := database.Put(k, v); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
		}
	}

	// Delete 40 keys (generating 40 DELETE records)
	for i := 0; i < 40; i++ {
		k := []byte(fmt.Sprintf("key_%03d", i))
		if err := database.Delete(k); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

	uncompactedSize := database.FileSize()

	// Execute Compaction
	if err := database.Compact(); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	compactedSize := database.FileSize()
	if compactedSize >= uncompactedSize {
		t.Fatalf("compactedSize (%d) not smaller than uncompactedSize (%d)", compactedSize, uncompactedSize)
	}

	// Verify key count is 60 (100 - 40 deleted)
	if database.KeyCount() != 60 {
		t.Fatalf("expected 60 live keys, got %d", database.KeyCount())
	}

	// Verify the 40 deleted keys return ErrNotFound
	for i := 0; i < 40; i++ {
		k := []byte(fmt.Sprintf("key_%03d", i))
		if _, err := database.Get(k); err != db.ErrNotFound {
			t.Fatalf("deleted key %s returned err=%v", k, err)
		}
	}

	// Verify the 60 remaining keys hold their latest round 4 value
	for i := 40; i < 100; i++ {
		k := []byte(fmt.Sprintf("key_%03d", i))
		expectedV := []byte(fmt.Sprintf("val_round_4_%03d", i))
		v, err := database.Get(k)
		if err != nil {
			t.Fatalf("Get key %s failed: %v", k, err)
		}
		if !bytes.Equal(v, expectedV) {
			t.Fatalf("value mismatch for key %s: got %s, expected %s", k, v, expectedV)
		}
	}

	// Verify check passes
	res, err := recovery.Check(dbPath)
	if err != nil || !res.Valid {
		t.Fatalf("Check failed on compacted file: %v, msg=%s", err, res.Message)
	}

	// Can write new keys after compaction
	if err := database.Put([]byte("new_post_compact"), []byte("fresh_val")); err != nil {
		t.Fatalf("Put after compaction failed: %v", err)
	}
	v, err := database.Get([]byte("new_post_compact"))
	if err != nil || string(v) != "fresh_val" {
		t.Fatalf("Get after compaction failed: %v", err)
	}

	_ = database.Close()
}

// Scenarios C1 to C5: Crash-safety during compaction
func TestCompaction_CrashScenarios_C1_to_C5(t *testing.T) {
	tempDir := t.TempDir()

	runSubprocessFailpoint := func(dbPath, fpName string) int {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperCompactionProcess")
		cmd.Env = append(os.Environ(),
			"GO_WANT_COMPACT_HELPER=1",
			"TEST_DB_PATH="+dbPath,
			"KVDB_FAILPOINT="+fpName,
		)
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode()
	}

	// Scenario C1: Crash before sync of tmp file
	t.Run("C1_CrashBeforeSync", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "c1.db")
		d, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		for i := 0; i < 20; i++ {
			_ = d.Put([]byte("c1_key_"+strconv.Itoa(i)), []byte("val_"+strconv.Itoa(i)))
		}
		_ = d.Close()

		exitCode := runSubprocessFailpoint(dbPath, "compaction_before_sync")
		if exitCode != 137 {
			t.Fatalf("expected exit 137, got %d", exitCode)
		}

		// Reopen must succeed cleanly
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Reopen after C1 failed: %v", err)
		}
		defer reopened.Close()

		for i := 0; i < 20; i++ {
			v, err := reopened.Get([]byte("c1_key_" + strconv.Itoa(i)))
			if err != nil || string(v) != "val_"+strconv.Itoa(i) {
				t.Fatalf("data lost after C1: %v", err)
			}
		}
	})

	// Scenario C2: Crash after sync, before rename
	t.Run("C2_CrashAfterSyncBeforeRename", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "c2.db")
		d, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		for i := 0; i < 20; i++ {
			_ = d.Put([]byte("c2_key_"+strconv.Itoa(i)), []byte("val_"+strconv.Itoa(i)))
		}
		_ = d.Close()

		exitCode := runSubprocessFailpoint(dbPath, "compaction_after_sync_before_rename")
		if exitCode != 137 {
			t.Fatalf("expected exit 137, got %d", exitCode)
		}

		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Reopen after C2 failed: %v", err)
		}
		defer reopened.Close()

		for i := 0; i < 20; i++ {
			v, err := reopened.Get([]byte("c2_key_" + strconv.Itoa(i)))
			if err != nil || string(v) != "val_"+strconv.Itoa(i) {
				t.Fatalf("data lost after C2: %v", err)
			}
		}
	})

	// Scenario C3: Crash after main renamed to bak, before tmp renamed to main
	t.Run("C3_CrashAfterRenameMainToBak", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "c3.db")
		d, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		for i := 0; i < 20; i++ {
			_ = d.Put([]byte("c3_key_"+strconv.Itoa(i)), []byte("val_"+strconv.Itoa(i)))
		}
		_ = d.Close()

		exitCode := runSubprocessFailpoint(dbPath, "compaction_after_rename_main_to_bak")
		if exitCode != 137 {
			t.Fatalf("expected exit 137, got %d", exitCode)
		}

		// Reopen must succeed cleanly by resolving bak/tmp
		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Reopen after C3 failed: %v", err)
		}
		defer reopened.Close()

		for i := 0; i < 20; i++ {
			v, err := reopened.Get([]byte("c3_key_" + strconv.Itoa(i)))
			if err != nil || string(v) != "val_"+strconv.Itoa(i) {
				t.Fatalf("data lost after C3: %v", err)
			}
		}
	})

	// Scenario C4: Crash after tmp renamed to main, before bak removed
	t.Run("C4_CrashAfterRenameTmpToMain", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "c4.db")
		d, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		for i := 0; i < 20; i++ {
			_ = d.Put([]byte("c4_key_"+strconv.Itoa(i)), []byte("val_"+strconv.Itoa(i)))
		}
		_ = d.Close()

		exitCode := runSubprocessFailpoint(dbPath, "compaction_after_rename_tmp_to_main")
		if exitCode != 137 {
			t.Fatalf("expected exit 137, got %d", exitCode)
		}

		reopened, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Reopen after C4 failed: %v", err)
		}
		defer reopened.Close()

		for i := 0; i < 20; i++ {
			v, err := reopened.Get([]byte("c4_key_" + strconv.Itoa(i)))
			if err != nil || string(v) != "val_"+strconv.Itoa(i) {
				t.Fatalf("data lost after C4: %v", err)
			}
		}
	})
}

// Concurrent readers during compaction: zero corruption, no panics
func TestCompaction_ConcurrentReaders(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "compact_concurrent.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Pre-seed 100 keys
	for i := 0; i < 100; i++ {
		_ = database.Put([]byte(fmt.Sprintf("k_%03d", i)), []byte(fmt.Sprintf("v_%03d", i)))
	}

	var wg sync.WaitGroup
	stopReaders := make(chan struct{})

	// 4 Readers
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			var count int
			for {
				select {
				case <-stopReaders:
					return
				default:
					idx := (readerID*17 + count) % 100
					k := fmt.Sprintf("k_%03d", idx)
					val, err := database.Get([]byte(k))
					if err != nil {
						t.Errorf("reader error on key %s: %v", k, err)
						return
					}
					if string(val) != fmt.Sprintf("v_%03d", idx) {
						t.Errorf("reader observed corrupt value: got %s", val)
						return
					}
					count++
				}
			}
		}(r)
	}

	// Trigger compaction while readers are active
	for c := 0; c < 3; c++ {
		if err := database.Compact(); err != nil {
			t.Fatalf("Concurrent compaction %d failed: %v", c, err)
		}
	}

	close(stopReaders)
	wg.Wait()

	// Check passes
	res, err := recovery.Check(dbPath)
	if err != nil || !res.Valid {
		t.Fatalf("Check failed: %v", err)
	}
}

func TestCompaction_EmptyDatabaseRemainsOpenable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-compact.db")
	d, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Compact(); err != nil {
		t.Fatalf("compact empty database: %v", err)
	}
	if d.KeyCount() != 0 {
		t.Fatalf("expected empty index, got %d keys", d.KeyCount())
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatalf("reopen empty compacted database: %v", err)
	}
	defer reopened.Close()
	if reopened.KeyCount() != 0 {
		t.Fatalf("expected empty reopened index, got %d", reopened.KeyCount())
	}
	if err := reopened.Put([]byte("after"), []byte("compact")); err != nil {
		t.Fatalf("write after empty compaction: %v", err)
	}
}

func TestCompaction_ChunksMoreThanMaximumBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large-compact.db")
	d, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := make([]db.BatchOp, 10000)
	for i := range first {
		key := []byte(fmt.Sprintf("key-%05d", i))
		first[i] = db.BatchOp{Type: db.OpPut, Key: key, Value: []byte("value")}
	}
	if err := d.Batch(first); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if err := d.Put([]byte("key-10000"), []byte("value")); err != nil {
		t.Fatalf("last key: %v", err)
	}
	if err := d.Compact(); err != nil {
		t.Fatalf("compact 10,001 keys: %v", err)
	}
	if d.KeyCount() != 10001 {
		t.Fatalf("expected 10001 keys after compaction, got %d", d.KeyCount())
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatalf("reopen after chunked compaction: %v", err)
	}
	defer reopened.Close()
	if reopened.KeyCount() != 10001 {
		t.Fatalf("expected 10001 keys after reopen, got %d", reopened.KeyCount())
	}
	got, err := reopened.Get([]byte("key-10000"))
	if err != nil || string(got) != "value" {
		t.Fatalf("last key lost after chunked compaction: %q, %v", got, err)
	}
}
