package tests_test

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/wal"
)

// TestBenchmark_1M_Keys_Recovery tests sequential recovery scan of 1,000,000 committed keys.
// The target SLA: recovery time must be under 2.0s.
func TestBenchmark_1M_Keys_Recovery(t *testing.T) {
	// Race instrumentation substantially changes allocation and CPU costs.
	if raceInstrumentationEnabled {
		t.Skip("performance SLA benchmark is not meaningful under the race detector")
	}
	if testing.Short() {
		t.Skip("skipping 1M benchmark in short mode")
	}

	const totalKeys = 1000000
	const chunkSize = 10000

	// Store the benchmark database permanently in the project root/data folder.
	// The Go test working directory is the tests directory.
	dataDir := filepath.Join("..", "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatalf("Create data directory failed: %v", err)
	}

	dbPath := filepath.Join(dataDir, "torq_1m.db")

	// This is a dedicated benchmark database.
	// Truncate it so old trailing data cannot remain after regeneration.
	f, err := os.OpenFile(
		dbPath,
		os.O_CREATE|os.O_RDWR|os.O_TRUNC,
		0644,
	)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	writeStart := time.Now()

	headerBuf := make([]byte, wal.FileHeaderSize)
	if err := wal.EncodeFileHeader(headerBuf); err != nil {
		_ = f.Close()
		t.Fatalf("Header encode failed: %v", err)
	}
	if _, err := f.Write(headerBuf); err != nil {
		_ = f.Close()
		t.Fatalf("Header write failed: %v", err)
	}

	val := []byte("v1234567890")
	recBuf := make([]byte, 4096)

	beginRec := &wal.Record{
		Type:    wal.RecordTypeBegin,
		OpCount: uint32(chunkSize),
	}
	commitRec := &wal.Record{
		Type:    wal.RecordTypeCommit,
		OpCount: uint32(chunkSize),
	}
	putRec := &wal.Record{
		Type:  wal.RecordTypePut,
		Value: val,
	}

	var batchBuf []byte

	for b := 0; b < totalKeys/chunkSize; b++ {
		batchBuf = batchBuf[:0]
		txnID := uint64(b + 1)

		// BEGIN record.
		beginRec.TxnID = txnID
		bLen, err := wal.EncodedRecordSize(beginRec)
		if err != nil {
			_ = f.Close()
			t.Fatalf("Begin record size failed: %v", err)
		}
		if len(recBuf) < bLen {
			recBuf = make([]byte, bLen)
		}
		if err := wal.EncodeRecordToBuf(beginRec, recBuf[:bLen]); err != nil {
			_ = f.Close()
			t.Fatalf("Begin record encode failed: %v", err)
		}
		batchBuf = append(batchBuf, recBuf[:bLen]...)

		// PUT records.
		putRec.TxnID = txnID

		for i := 0; i < chunkSize; i++ {
			k := b*chunkSize + i

			putRec.Key = []byte{
				'k',
				byte('0' + (k/1000000)%10),
				byte('0' + (k/100000)%10),
				byte('0' + (k/10000)%10),
				byte('0' + (k/1000)%10),
				byte('0' + (k/100)%10),
				byte('0' + (k/10)%10),
				byte('0' + k%10),
			}

			pLen, err := wal.EncodedRecordSize(putRec)
			if err != nil {
				_ = f.Close()
				t.Fatalf("PUT record size failed: %v", err)
			}
			if len(recBuf) < pLen {
				recBuf = make([]byte, pLen)
			}
			if err := wal.EncodeRecordToBuf(putRec, recBuf[:pLen]); err != nil {
				_ = f.Close()
				t.Fatalf("PUT record encode failed: %v", err)
			}
			batchBuf = append(batchBuf, recBuf[:pLen]...)
		}

		// COMMIT record.
		commitRec.TxnID = txnID
		cLen, err := wal.EncodedRecordSize(commitRec)
		if err != nil {
			_ = f.Close()
			t.Fatalf("Commit record size failed: %v", err)
		}
		if len(recBuf) < cLen {
			recBuf = make([]byte, cLen)
		}
		if err := wal.EncodeRecordToBuf(commitRec, recBuf[:cLen]); err != nil {
			_ = f.Close()
			t.Fatalf("Commit record encode failed: %v", err)
		}
		batchBuf = append(batchBuf, recBuf[:cLen]...)

		if _, err := f.Write(batchBuf); err != nil {
			_ = f.Close()
			t.Fatalf("Write failed: %v", err)
		}
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatalf("Sync failed: %v", err)
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		t.Fatalf("Stat failed: %v", err)
	}
	fileSize := fi.Size()

	if err := f.Close(); err != nil {
		t.Fatalf("Close benchmark file failed: %v", err)
	}

	writeDuration := time.Since(writeStart)

	t.Logf("Finished writing %d keys in %v (throughput: %.0f ops/sec)",
		totalKeys,
		writeDuration,
		float64(totalKeys)/writeDuration.Seconds(),
	)
	t.Logf("Database file: %s", dbPath)
	t.Logf("Database file size: %d MB", fileSize/(1024*1024))

	// Benchmark recovery scan time.
	t.Log("Starting recovery scan benchmark on 1,000,000 keys...")

	recoverStart := time.Now()

	recoveredDB, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Recovery Open failed: %v", err)
	}

	recoverDuration := time.Since(recoverStart)
	defer recoveredDB.Close()

	t.Logf("Recovery completed in %v!", recoverDuration)
	t.Logf("Recovered key count: %d", recoveredDB.KeyCount())

	if recoveredDB.KeyCount() != totalKeys {
		t.Fatalf("expected %d keys, got %d", totalKeys, recoveredDB.KeyCount())
	}

	// Verify target SLA: recovery < 2.0s.
	if recoverDuration > 2*time.Second {
		t.Errorf("Recovery took %v, which exceeds the 2.0s SLA!", recoverDuration)
	}

	// Verify first, middle, and last keys.
	sampleKeys := []string{
		"k0000000",
		"k0500000",
		"k0999999",
	}

	for _, key := range sampleKeys {
		sampleVal, err := recoveredDB.Get([]byte(key))
		if err != nil {
			t.Fatalf("Sample key %s get failed: %v", key, err)
		}
		if string(sampleVal) != string(val) {
			t.Fatalf("Sample key %s has unexpected value: %q", key, sampleVal)
		}
	}

	t.Log("First, middle, and last keys verified successfully.")
}

// BenchmarkBatchWrites measures throughput of multi-operation atomic batches.
func BenchmarkBatchWrites_100Ops(b *testing.B) {
	tempDir := b.TempDir()
	dbPath := filepath.Join(tempDir, "bench_batch.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	const batchSize = 100
	val := []byte("benchmark_value_payload")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ops := make([]db.BatchOp, batchSize)

		for j := 0; j < batchSize; j++ {
			ops[j] = db.BatchOp{
				Type:  db.OpPut,
				Key:   []byte(fmt.Sprintf("bench_k_%d_%d", i, j)),
				Value: val,
			}
		}

		if err := database.Batch(ops); err != nil {
			b.Fatalf("Batch failed: %v", err)
		}
	}

	b.ReportMetric(float64(b.N*batchSize)/b.Elapsed().Seconds(), "keys/sec")
}

// BenchmarkPointGet measures latency of single-key point reads from disk via ReadAt.
func BenchmarkPointGet(b *testing.B) {
	tempDir := b.TempDir()
	dbPath := filepath.Join(tempDir, "bench_get.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Pre-seed 10,000 keys.
	const keyCount = 10000

	ops := make([]db.BatchOp, keyCount)

	for i := 0; i < keyCount; i++ {
		ops[i] = db.BatchOp{
			Type:  db.OpPut,
			Key:   []byte(fmt.Sprintf("get_key_%05d", i)),
			Value: []byte(fmt.Sprintf("val_%05d", i)),
		}
	}

	if err := database.Batch(ops); err != nil {
		b.Fatalf("Pre-seed failed: %v", err)
	}

	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 1000)

	for i := 0; i < 1000; i++ {
		keys[i] = []byte(fmt.Sprintf("get_key_%05d", r.Intn(keyCount)))
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		k := keys[i%1000]

		if _, err := database.Get(k); err != nil {
			b.Fatalf("Get failed: %v", err)
		}
	}
}

// BenchmarkRecoveryScan_100K measures the recovery scan speed on 100,000 pre-committed keys.
func BenchmarkRecoveryScan_100K(b *testing.B) {
	tempDir := b.TempDir()
	dbPath := filepath.Join(tempDir, "bench_rec_100k.db")

	database, err := db.Open(dbPath, db.Options{})
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}

	const totalKeys = 100000
	const batchSize = 10000

	val := []byte("val100k")

	for i := 0; i < totalKeys/batchSize; i++ {
		ops := make([]db.BatchOp, batchSize)

		for j := 0; j < batchSize; j++ {
			ops[j] = db.BatchOp{
				Type:  db.OpPut,
				Key:   []byte(fmt.Sprintf("rec_%06d", i*batchSize+j)),
				Value: val,
			}
		}

		if err := database.Batch(ops); err != nil {
			b.Fatalf("Batch failed: %v", err)
		}
	}

	if err := database.Close(); err != nil {
		b.Fatalf("Close failed: %v", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		recDB, err := db.Open(dbPath, db.Options{})
		if err != nil {
			b.Fatalf("Open recovery failed: %v", err)
		}

		if recDB.KeyCount() != totalKeys {
			b.Fatalf("Key count mismatch: %d", recDB.KeyCount())
		}

		if err := recDB.Close(); err != nil {
			b.Fatalf("Close recovery DB failed: %v", err)
		}
	}

	b.ReportMetric(float64(totalKeys*b.N)/b.Elapsed().Seconds(), "keys_scanned/sec")
}
