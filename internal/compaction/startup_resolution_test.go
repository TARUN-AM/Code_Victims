package compaction_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TARUN-AM/torq/internal/compaction"
	"github.com/TARUN-AM/torq/internal/db"
)

func TestStartupResolutionRestoresValidatedBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	d, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put([]byte("important"), []byte("known-good")); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+compaction.BakSuffix, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a valid database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+compaction.TmpSuffix, []byte("invalid temp"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := compaction.ResolveStartupResolution(path); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatal("validated backup was not restored")
	}
	reopened, err := db.Open(path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	value, err := reopened.Get([]byte("important"))
	if err != nil || string(value) != "known-good" {
		t.Fatalf("restored data unavailable: %q, %v", value, err)
	}
}

func TestStartupResolutionPreservesArtifactsWhenNoCopyValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	for _, suffix := range []string{"", compaction.TmpSuffix, compaction.BakSuffix} {
		if err := os.WriteFile(path+suffix, []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := compaction.ResolveStartupResolution(path); err == nil {
		t.Fatal("expected error when no valid copy exists")
	}
	for _, suffix := range []string{"", compaction.TmpSuffix, compaction.BakSuffix} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("recovery deleted diagnostic artifact %q: %v", suffix, err)
		}
	}
}
