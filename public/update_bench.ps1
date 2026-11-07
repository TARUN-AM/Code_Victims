# PowerShell helper script to update tests\benchmark_test.go directly
$benchContent = @'
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

// BenchmarkRecovery1MKeys tests sequential recovery scan of 1,000,000 committed keys.
// The target SLA: recovery time must be under 2.0s.
func TestBenchmark_1M_Keys_Recovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M benchmark in short mode")
	}

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "bench_1m.db")

	const totalKeys = 1000000
	const chunkSize = 100000

	t.Logf("Generating and streaming %d keys to %s...", totalKeys, dbPath)
	writeStart := time.Now()

	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	headerBuf := make([]byte, wal.FileHeaderSize)
	if err := wal.EncodeFileHeader(headerBuf); err != nil {
		t.Fatalf("Header encode failed: %v", err)
	}
	if _, err := f.Write(headerBuf); err != nil {
		t.Fatalf("Header write failed: %v", err)
	}

	val := []byte("v1234567890")
	recBuf := make([]byte, 4096)

	beginRec := &wal.Record{Type: wal.RecordTypeBegin, OpCount: uint32(chunkSize)}
	commitRec := &wal.Record{Type: wal.RecordTypeCommit, OpCount: uint32(chunkSize)}
	putRec := &wal.Record{Type: wal.RecordTypePut, Value: val}

	var batchBuf []byte

	for b := 0; b < totalKeys/chunkSize; b++ {
		batchBuf = batchBuf[:0]
		txnID := uint64(b + 1)

		beginRec.TxnID = txnID
		bLen, _ := wal.EncodedRecordSize(beginRec)
		if len(recBuf) < bLen {
			recBuf = make([]byte, bLen)
		}
		_ = wal.EncodeRecordToBuf(beginRec, recBuf[:bLen])
		batchBuf = append(batchBuf, recBuf[:bLen]...)

		putRec.TxnID = txnID
		for i := 0; i < chunkSize; i++ {
			k := b*chunkSize + i
			putRec.Key = []byte{'k',
				byte('0' + (k/1000000)%10),
				byte('0' + (k/100000)%10),
				byte('0' + (k/10000)%10),
				byte('0' + (k/1000)%10),
				byte('0' + (k/100)%10),
				byte('0' + (k/10)%10),
				byte('0' + (k % 10)),
			}
			pLen, _ := wal.EncodedRecordSize(putRec)
			if len(recBuf) < pLen {
				recBuf = make([]byte, pLen)
			}
			_ = wal.EncodeRecordToBuf(putRec, recBuf[:pLen])
			batchBuf = append(batchBuf, recBuf[:pLen]...)
		}

		commitRec.TxnID = txnID
		cLen, _ := wal.EncodedRecordSize(commitRec)
		if len(recBuf) < cLen {
			recBuf = make([]byte, cLen)
		}
		_ = wal.EncodeRecordToBuf(commitRec, recBuf[:cLen])
		batchBuf = append(batchBuf, recBuf[:cLen]...)

		if _, err := f.Write(batchBuf); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	fileSize := fi.Size()
	f.Close()

	writeDuration := time.Since(writeStart)
	t.Logf("Finished writing %d keys in %v (throughput: %.0f ops/sec)",
		totalKeys, writeDuration, float64(totalKeys)/writeDuration.Seconds())
	t.Logf("Database file size: %d MB", fileSize/(1024*1024))

	// Now benchmark recovery scan time
	t.Logf("Starting recovery scan benchmark on 1,000,000 keys...")
	recoverStart := time.Now()

	recoveredDB, err := db.Open(dbPath, db.Options{})
	if err != nil {
		t.Fatalf("Recovery Open failed: %v", err)
	}
	recoverDuration := time.Since(recoverStart)
	defer recoveredDB.Close()

	t.Logf("Recovery completed in %v!", recoverDuration)

	if recoveredDB.KeyCount() != totalKeys {
		t.Fatalf("expected %d keys, got %d", totalKeys, recoveredDB.KeyCount())
	}

	// Verify target SLA: recovery < 2.0s
	if recoverDuration > 2*time.Second {
		t.Errorf("Recovery took %v, which exceeds the 2.0s SLA!", recoverDuration)
	}

	// Quick sample validation
	sampleVal, err := recoveredDB.Get([]byte("k0500000"))
	if err != nil || string(sampleVal) != string(val) {
		t.Fatalf("Sample key get failed: %v", err)
	}
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

	// Pre-seed 10,000 keys
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
		_, err := database.Get(k)
		if err != nil {
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
	_ = database.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recDB, err := db.Open(dbPath, db.Options{})
		if err != nil {
			b.Fatalf("Open recovery failed: %v", err)
		}
		if recDB.KeyCount() != totalKeys {
			b.Fatalf("Key count mismatch: %d", recDB.KeyCount())
		}
		_ = recDB.Close()
	}
	b.ReportMetric(float64(totalKeys*b.N)/b.Elapsed().Seconds(), "keys_scanned/sec")
}
'@

[System.IO.File]::WriteAllText("tests\benchmark_test.go", $benchContent)
Write-Host "[SUCCESS] Updated tests\benchmark_test.go successfully!" -ForegroundColor Green
