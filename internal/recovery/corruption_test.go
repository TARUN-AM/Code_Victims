package recovery_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/recovery"
	"github.com/TARUN-AM/torq/internal/wal"
)

func TestRecoverFailsClosedOnCompleteChecksumCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	d, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put([]byte("alpha"), []byte("bravo")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put([]byte("later"), []byte("committed-after-alpha")); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the first transaction's COMMIT checksum while retaining
	// a later committed transaction. This is middle-file corruption, not a
	// possibly interrupted final append.
	firstBeginLen := 18 + 4
	firstPutBody := 6 + len("alpha") + len("bravo")
	firstPutLen := 18 + firstPutBody
	firstCommitLen := 18 + 4
	firstCommitEnd := wal.FileHeaderSize + firstBeginLen + firstPutLen + firstCommitLen
	before[firstCommitEnd-1] ^= 0x80
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	damaged, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, recoverErr := recovery.Recover(f)
	_ = f.Close()
	if recoverErr == nil {
		t.Fatal("expected recovery to reject checksum corruption")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != damaged.Size() {
		t.Fatalf("recovery modified corrupt file: before=%d after=%d", damaged.Size(), after.Size())
	}
}
