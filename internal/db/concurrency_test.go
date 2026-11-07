package db_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/recovery"
)

// Helper to construct a self-verifying payload:
// Format: Key(string) + Version(uint32) + Payload + CRC32(uint32)
func makePayload(key string, version uint32, size int) []byte {
	buf := make([]byte, size)
	copy(buf, key)
	offset := len(key)
	if offset+8 > size {
		offset = 0
	}
	binary.LittleEndian.PutUint32(buf[offset:offset+4], version)
	// Last 4 bytes = CRC32 of everything preceding
	crc := crc32.ChecksumIEEE(buf[:size-4])
	binary.LittleEndian.PutUint32(buf[size-4:], crc)
	return buf
}

func verifyPayload(t *testing.T, key string, val []byte) {
	if len(val) < 8 {
		t.Fatalf("corrupt read: payload for %q too small (%d bytes)", key, len(val))
	}
	expectedCRC := binary.LittleEndian.Uint32(val[len(val)-4:])
	actualCRC := crc32.ChecksumIEEE(val[:len(val)-4])
	if actualCRC != expectedCRC {
		t.Fatalf("corrupt read for %q: CRC mismatch (expected %08x, got %08x)", key, expectedCRC, actualCRC)
	}
}

// 8 reader goroutines + 4 writer goroutines running for 2 seconds.
// Writers do Put/Delete/Batch. Readers do Get and verify:
// (a) never get an empty/corrupt read;
// (b) if a key is found, its value matches what a writer wrote (CRC32 validated);
// (c) batch operations observe all-or-nothing.
func TestConcurrentReadWrite_NoCorruption(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "concurrent_rw.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	stopSignal := make(chan struct{})
	var writeCount uint64
	var readCount uint64

	numWriters := 4
	numReaders := 8
	var wg sync.WaitGroup

	// Pre-seed 100 keys
	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("key_%04d", i)
		v := makePayload(k, 1, 64)
		if err := database.Put([]byte(k), v); err != nil {
			t.Fatalf("pre-seed failed: %v", err)
		}
	}

	// 4 Writers
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(workerID * 999)))
			var ver uint32 = 2

			for {
				select {
				case <-stopSignal:
					return
				default:
					ver++
					opType := r.Intn(3)
					kIdx := r.Intn(100)
					k := fmt.Sprintf("key_%04d", kIdx)

					switch opType {
					case 0:
						// Put
						payload := makePayload(k, ver, 32+r.Intn(128))
						if err := database.Put([]byte(k), payload); err != nil {
							if !errors.Is(err, db.ErrClosed) {
								t.Errorf("writer put error: %v", err)
							}
							return
						}
						atomic.AddUint64(&writeCount, 1)

					case 1:
						// Delete
						if err := database.Delete([]byte(k)); err != nil {
							if !errors.Is(err, db.ErrClosed) {
								t.Errorf("writer del error: %v", err)
							}
							return
						}
						atomic.AddUint64(&writeCount, 1)

					case 2:
						// Atomic Batch of 3 keys (either all 3 exist with same batchID, or none)
						batchID := ver
						bOps := make([]db.BatchOp, 3)
						for b := 0; b < 3; b++ {
							bk := fmt.Sprintf("paired_b%d_%d", workerID, b)
							bv := makePayload(bk, batchID, 48)
							bOps[b] = db.BatchOp{Type: db.OpPut, Key: []byte(bk), Value: bv}
						}
						if err := database.Batch(bOps); err != nil {
							if !errors.Is(err, db.ErrClosed) {
								t.Errorf("writer batch error: %v", err)
							}
							return
						}
						atomic.AddUint64(&writeCount, 1)
					}
				}
			}
		}(w)
	}

	// 8 Readers
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 1234)))

			for {
				select {
				case <-stopSignal:
					return
				default:
					// Check a random key from 0..99
					kIdx := rng.Intn(100)
					k := fmt.Sprintf("key_%04d", kIdx)
					val, err := database.Get([]byte(k))
					if err != nil {
						if !errors.Is(err, db.ErrNotFound) && !errors.Is(err, db.ErrClosed) {
							t.Errorf("reader get error: %v", err)
							return
						}
					} else {
						// (a) never an empty read
						if len(val) == 0 {
							t.Errorf("empty read for existing key %q", k)
						}
						// (b) CRC32 validity check
						verifyPayload(t, k, val)
					}

					// (c) Check atomic batch pair integrity
					pairedWorker := rng.Intn(numWriters)
					p0Key := fmt.Sprintf("paired_b%d_0", pairedWorker)
					p1Key := fmt.Sprintf("paired_b%d_1", pairedWorker)

					v0, err0 := database.Get([]byte(p0Key))
					v1, err1 := database.Get([]byte(p1Key))

					if err0 == nil && err1 == nil {
						// Both present: verify CRC
						verifyPayload(t, p0Key, v0)
						verifyPayload(t, p1Key, v1)
					} else if err0 != nil && err1 != nil {
						// Both absent: valid atomic state
					} else {
						// One was present and one was absent.
						// Because two separate Get calls have a time gap between them,
						// a batch could have committed during that gap.
						// Re-read the missing key to distinguish concurrent commit from a torn batch.
						if err0 != nil {
							if _, retryErr := database.Get([]byte(p0Key)); retryErr != nil && !errors.Is(retryErr, db.ErrClosed) {
								t.Errorf("D4 violation: atomic batch torn! key0 missing (%v), key1 present", retryErr)
							}
						} else if err1 != nil {
							if _, retryErr := database.Get([]byte(p1Key)); retryErr != nil && !errors.Is(retryErr, db.ErrClosed) {
								t.Errorf("D4 violation: atomic batch torn! key1 missing (%v), key0 present", retryErr)
							}
						}
					}

					atomic.AddUint64(&readCount, 1)
				}
			}
		}(r)
	}

	// Run for 2 seconds
	time.Sleep(2 * time.Second)
	close(stopSignal)
	wg.Wait()

	t.Logf("Concurrency test completed successfully: %d writes, %d reads under lock",
		atomic.LoadUint64(&writeCount), atomic.LoadUint64(&readCount))
}

// Close during active writes: must not panic or corrupt; subsequent operations return ErrClosed.
func TestCloseDuringActiveConcurrentWrites(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "close_active.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	stopSignal := make(chan struct{})
	var wg sync.WaitGroup

	// Launch 6 aggressive writers
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			var n int
			for {
				select {
				case <-stopSignal:
					return
				default:
					n++
					k := []byte(fmt.Sprintf("k_%d_%d", workerID, n))
					v := makePayload(string(k), uint32(n), 64)
					err := database.Put(k, v)
					if err != nil {
						if !errors.Is(err, db.ErrClosed) {
							t.Errorf("unexpected error before/during close: %v", err)
						}
						return
					}
				}
			}
		}(i)
	}

	// Allow writers to run briefly
	time.Sleep(100 * time.Millisecond)

	// Close during active concurrent writes
	if err := database.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	close(stopSignal)
	wg.Wait()

	// All subsequent operations MUST return ErrClosed
	if err := database.Put([]byte("after"), []byte("fail")); !errors.Is(err, db.ErrClosed) {
		t.Fatalf("expected ErrClosed on Put after Close, got %v", err)
	}
	if _, err := database.Get([]byte("after")); !errors.Is(err, db.ErrClosed) {
		t.Fatalf("expected ErrClosed on Get after Close, got %v", err)
	}
	if err := database.Delete([]byte("after")); !errors.Is(err, db.ErrClosed) {
		t.Fatalf("expected ErrClosed on Delete after Close, got %v", err)
	}

	// Reopen must succeed and verify clean recovery
	reopened, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Reopening database after concurrent Close failed: %v", err)
	}
	defer reopened.Close()

	// Verify database integrity via check
	res, err := recovery.Check(dbPath)
	if err != nil || !res.Valid {
		t.Fatalf("database corrupt after Close during active writes: %v, msg=%s", err, res.Message)
	}
}

// Scenarios D1 to D4
func TestScenarios_D1_to_D4(t *testing.T) {
	tempDir := t.TempDir()

	// D1: Two writers, different keys
	t.Run("D1_TwoWritersDifferentKeys", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "d1.db")
		database, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer database.Close()

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = database.Put([]byte("w1_key_"+strconv.Itoa(i)), []byte("val_w1"))
			}
		}()

		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = database.Put([]byte("w2_key_"+strconv.Itoa(i)), []byte("val_w2"))
			}
		}()

		wg.Wait()

		for i := 0; i < 50; i++ {
			v1, err1 := database.Get([]byte("w1_key_" + strconv.Itoa(i)))
			v2, err2 := database.Get([]byte("w2_key_" + strconv.Itoa(i)))
			if err1 != nil || string(v1) != "val_w1" {
				t.Fatalf("w1 key missing: %v", err1)
			}
			if err2 != nil || string(v2) != "val_w2" {
				t.Fatalf("w2 key missing: %v", err2)
			}
		}
	})

	// D2: Two writers, same key (serial execution, last commit wins, zero torn values)
	t.Run("D2_TwoWritersSameKey", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "d2.db")
		database, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer database.Close()

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				val := makePayload("contested_key", uint32(i), 64)
				_ = database.Put([]byte("contested_key"), val)
			}
		}()

		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				val := makePayload("contested_key", uint32(i+1000), 64)
				_ = database.Put([]byte("contested_key"), val)
			}
		}()

		wg.Wait()

		// Read contested key: value must be 100% valid with correct CRC (never torn)
		val, err := database.Get([]byte("contested_key"))
		if err != nil {
			t.Fatalf("Get contested_key failed: %v", err)
		}
		verifyPayload(t, "contested_key", val)
	})

	// D3: Long batch plus single puts
	t.Run("D3_LongBatchPlusSinglePuts", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "d3.db")
		database, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer database.Close()

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: 500-op batch
		go func() {
			defer wg.Done()
			batch := make([]db.BatchOp, 500)
			for i := 0; i < 500; i++ {
				batch[i] = db.BatchOp{
					Type:  db.OpPut,
					Key:   []byte("batch_item_" + strconv.Itoa(i)),
					Value: []byte("batch_val_" + strconv.Itoa(i)),
				}
			}
			if err := database.Batch(batch); err != nil {
				t.Errorf("batch failed: %v", err)
			}
		}()

		// Goroutine 2: Individual puts
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = database.Put([]byte("single_put_"+strconv.Itoa(i)), []byte("single_val"))
			}
		}()

		wg.Wait()

		// Check all 500 batch keys exist
		for i := 0; i < 500; i++ {
			v, err := database.Get([]byte("batch_item_" + strconv.Itoa(i)))
			if err != nil || string(v) != "batch_val_"+strconv.Itoa(i) {
				t.Fatalf("missing batch item %d: %v", i, err)
			}
		}
	})

	// D4: Reader during batch (all-or-nothing atomicity check)
	t.Run("D4_ReaderDuringBatch", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "d4.db")
		database, err := db.Open(dbPath, db.Options{})
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer database.Close()

		var stop atomic.Bool
		var readerWg sync.WaitGroup
		readerWg.Add(1)

		go func() {
			defer readerWg.Done()
			for !stop.Load() {
				// Reader checks all 10 keys: either all 10 exist, or none exist
				var foundCount int
				for i := 0; i < 10; i++ {
					_, err := database.Get([]byte("atomic_b_" + strconv.Itoa(i)))
					if err == nil {
						foundCount++
					}
				}
				if foundCount != 0 && foundCount != 10 {
					// Verify if the batch was simply committed midway through the 10 Get calls
					var secondPass int
					for i := 0; i < 10; i++ {
						if _, err := database.Get([]byte("atomic_b_" + strconv.Itoa(i))); err == nil {
							secondPass++
						}
					}
					if secondPass != 10 {
						t.Errorf("D4 violation: observed partial batch! found %d of 10 keys (second pass: %d)", foundCount, secondPass)
						return
					}
				}
			}
		}()

		// Run 5 successive batches
		for b := 0; b < 5; b++ {
			batch := make([]db.BatchOp, 10)
			for i := 0; i < 10; i++ {
				batch[i] = db.BatchOp{
					Type:  db.OpPut,
					Key:   []byte("atomic_b_" + strconv.Itoa(i)),
					Value: []byte("val_" + strconv.Itoa(b)),
				}
			}
			_ = database.Batch(batch)
		}

		stop.Store(true)
		readerWg.Wait()
	})
}
