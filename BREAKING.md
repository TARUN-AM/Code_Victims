# TORQ Breaking Decisions & Reproducibility Guide
**Event**: Kill It and Prove It (24-Hour Hackathon)  
**Status**: Frozen & Verified  
**Zero-Dependency Guarantee**: Pure Go Standard Library & Python 3 Standard Library only.

---

## 1. Zero-Dependency Manifesto

TORQ was conceived under a strict hackathon constraint: **zero external dependencies**.

- **Go Database Engine**:
  ```text
  module github.com/TARUN-AM/torq
  go 1.22
  ```
  `go.mod` contains **zero `require` statements**. It compiles in air-gapped environments with `GOPROXY=off` without fetching a single remote package.
- **Python Correctness Oracle (`torture/torture.py`)**:
  Requires only standard library modules: `argparse`, `binascii`, `os`, `platform`, `random`, `shutil`, `signal`, `subprocess`, `sys`, `tempfile`, `threading`, `time`, `typing`. Zero `pip install` required.
- **Cross-Platform Launchers**:
  Shell scripts (`kvdb`, `run_torture.sh`) and Windows batch files (`kvdb.cmd`, `torture.cmd`) run natively on POSIX and Windows PowerShell/Command Prompt.

---

## 2. Intentional Architectural Deviations (Why We Broke From "Standard" Naive Designs)

### Deviation 1: Values Never Reside in RAM (Direct-from-Disk ReadAt)
* **Naive Approach**: Many embedded databases cache key-value pairs in memory (or keep an in-memory copy of values).
* **The Failure Mode**: Caching values in memory introduces memory exhaustion on large datasets (e.g. 1 MB values or 1,000,000 keys) and creates subtle state drift between RAM cache and disk under process kills.
* **TORQ's Decision**: Values are **never cached in RAM**. The in-memory index stores only 12-byte `(ValueOffset: int64, ValueLength: uint32)` records alongside the string key.
* **Benefit**: Large values are not duplicated in the in-memory index. Actual memory use still depends on key lengths, Go map overhead, and allocator behavior; measure it on the target machine. Point reads use `file.ReadAt` and are serialized against compaction by the database read lock.

---

### Deviation 2: Two-Phase Write & Sync Protocol (Double Fsync)
* **Naive Approach**: Append `BEGIN`, ops, and `COMMIT` to disk buffer, then call `file.Sync()` once at the end.
* **The Failure Mode**: Modern storage controllers and solid-state drive caches aggressively reorder un-flushed writes. If a sudden power loss occurs between writes, a drive controller may flush the `COMMIT` record to flash media while the preceding `PUT` data payload is still lingering in volatile controller cache. On recovery, the database sees a valid `COMMIT` pointing to corrupt or zeroed bytes.
* **TORQ's Decision**: Every transaction follows a strict two-phase sync protocol:
  1. Stream `BEGIN` and payload operations (`PUT`, `DELETE`).
  2. **Phase 1 Sync**: `file.Sync()` physically forces all payload bytes onto non-volatile media.
  3. Stream `COMMIT` record.
  4. **Phase 2 Sync**: `file.Sync()` physically locks the commit record onto media.
  5. Apply committed operations to the in-memory index and return client ACK.
* **Benefit**: Orders payload synchronization before the commit record is written and synced. This is scoped to the specified process-kill model; it is not a power-loss or hardware-failure guarantee.

---

### Deviation 3: Three-Step Compaction Swap with C1–C5 Resolution Matrix
* **Naive Approach**: Write `main.db.tmp`, then execute `os.Rename("main.db.tmp", "main.db")`.
* **The Failure Mode**: On Windows filesystems (NTFS), `os.Rename` fails immediately with `Access is denied` or `File already exists` if the destination file exists or is currently open by any thread. Furthermore, if a crash occurs mid-rename on POSIX, incomplete directory metadata can orphan records.
* **TORQ's Decision**: Compaction executes an explicit 3-step atomic file swap:
  1. `file.Close()` closes the active database file descriptor. The database exclusive lock and OS-level sidecar lock remain held throughout.
  2. Step A: `main.db` $\to$ `main.db.compact.bak`
  3. Step B: `main.db.compact.tmp` $\to$ `main.db`
  4. Step C: Remove `main.db.compact.bak`
  5. Reopen `main.db`.
  On startup, `ResolveStartupResolution(path)` inspects the `{main, tmp, bak}` file set and cleanly resolves all 5 possible crash interruption points (C1–C5).
* **Benefit**: startup validates complete committed contents of main/temp/backup candidates and preserves artifacts if no valid copy exists. The rename workflow has automated crash tests in this environment; runtime validation on the target Windows machine is still required.

---

### Deviation 4: ASCII Line-Based Hex Streaming Protocol for `kvdb serve`
* **Naive Approach**: Using JSON, Protobuf, or gRPC for client-server protocol.
* **The Failure Mode**: JSON requires escaping null bytes and binary payloads, incurring severe serialization overhead; Protobuf and gRPC require non-standard compilers, code generation, and external runtime packages.
* **TORQ's Decision**: `kvdb serve` implements a clean ASCII line-based hex protocol over `stdin`/`stdout`:
  ```text
  PUT <id> <hex_key> <hex_val>
  GET <id> <hex_key>
  DEL <id> <hex_key>
  BATCH <id> <count> <put|del> <hex_key> [<hex_val>] ...
  COMPACT <id>
  ```
* **Benefit**: Zero dependencies, transparent framing, arbitrary binary key/value support (including null bytes and 1 MB blobs), and immune to shell/stdio newline confusion.

---

### Deviation 5: Preflight Capacity Calculation (Byte-Identical Rejection)
* **Naive Approach**: Start appending bytes to the file until an `ENOSPC` / disk full error is returned by `file.Write()`, then attempt to truncate or roll back.
* **The Failure Mode**: Appending partial records before catching a disk full error mutates file length, burns disk write cycles, and leaves torn fragments at EOF that force recovery truncation on next open.
* **TORQ's Decision**: `internal/limits/guard.go` pre-computes the exact byte footprint (`EncodedTxnOverhead + sum(op_sizes)`) under exclusive lock before writing a single byte. If `currentOffset + totalTxnSize > maxBytes`, it immediately rejects the call with `limits.ErrDiskFull`.
* **Benefit**: Disk-full rejections (Scenarios E1 and E2) leave the database file **100% byte-for-byte identical**.

---

## 3. Additional Hardening Applied
- Compaction writes bounded committed chunks of at most 10,000 live keys each, preserving the normal transaction limit even for databases larger than 10,000 keys. Empty compaction produces a valid header-only file.
- A stable `<dbPath>.lock` sidecar is exclusively locked at the OS level before startup recovery/compaction resolution. It prevents a second process from opening the same database; process termination releases the OS lock.
- `Batch()` rejects unknown operation types and DELETE operations carrying values, and clones caller-owned key/value buffers before writing.
- Recovery bounds BEGIN operation counts to 1..10,000 and rejects non-monotonic transaction IDs, nested transactions, mismatched operation counts, and corruption before later WAL bytes.
- A malformed/checksum-invalid frame is only treated as an interrupted tail when its declared frame ends exactly at EOF. Middle-file corruption fails closed.
- Compaction startup recovery validates the full committed file via the read-only recovery checker. It prefers a validated main file, otherwise promotes a validated temp file or restores a validated backup; if none validate, it preserves all artifacts and returns an error.
- Duplicate CLI and benchmark Go files were removed from `public/`; the canonical CLI remains in `internal/cli/`, and the benchmark remains in `tests/`, so `go test ./...` is the standard project-wide command.
- The 1M-key performance test is skipped under `-race` because race instrumentation invalidates the 2-second performance measurement. Run it separately without `-race`.

## 4. Clean Clone & Reproducible Build Instructions

### Linux / macOS:
```bash
# 1. Clone repository
git clone https://github.com/TARUN-AM/torq.git
cd torq

# 2. Build without network (pure standard library)
GOPROXY=off go build -o bin/kvdb ./cmd/kvdb

# 3. Run full unit and integration test suite
go test ./... -v -short

# 4. Run 50-round process-kill torture test
./run_torture.sh --rounds 50 --ops 50

# 5. Run 1,000,000 keys recovery benchmark
go test -v ./tests -run TestBenchmark_1M_Keys_Recovery -timeout 60s
```

### Windows (PowerShell):
```powershell
# 1. Navigate to repository root
cd C:\Users\ASUS\torq

# 2. Build kvdb executable
$env:GOPROXY="off"
go build -o bin\kvdb.exe .\cmd\kvdb

# 3. Run all tests
go test .\... -v -short

# 4. Run torture harness
python torture\torture.py --rounds 50 --ops 50

# 5. Run 1,000,000 keys recovery benchmark
go test -v .\tests -run TestBenchmark_1M_Keys_Recovery -timeout 60s
```
