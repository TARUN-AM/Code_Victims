# TORQ Engineering Hardening Report

Review baseline: the uploaded `final.zip` source tree. The existing WAL format, database API, append-only storage model, in-memory offset index, CLI, Python oracle, and React demo were retained. Changes were targeted at correctness, bounded recovery, compaction, process ownership, and reproducibility.

## Changes applied

1. **Cross-process single-owner lock**
   - `db.Open()` acquires an exclusive OS lock on a stable `<dbPath>.lock` sidecar before compaction recovery or database-file access.
   - POSIX uses non-blocking `flock`; Windows uses `LockFileEx` with fail-immediately semantics.
   - The sidecar remains stable across compaction renames. `Close()` releases the lock; the OS releases it after abrupt process termination.

2. **Strict API input ownership and validation**
   - Unknown batch operation types are rejected rather than being interpreted as DELETE.
   - DELETE operations containing a value are rejected.
   - `Batch`, `Put`, and `Delete` copy caller-owned key/value buffers before writing.

3. **Recovery fail-closed behavior and bounds**
   - BEGIN operation counts are limited to 1..10,000 before allocating staging memory.
   - Transaction IDs must be non-zero and strictly increasing.
   - Nested transactions, mismatched IDs, excess operations, and malformed transaction framing are rejected.
   - Incomplete final records and malformed frames that exactly occupy the physical EOF tail can be repaired to the last committed offset. Corruption before later bytes is reported and is not silently truncated.

4. **Compaction startup validation**
   - Startup validates complete committed contents (header, record checksums, transaction framing) of main/temp/backup candidates.
   - A valid main file is preferred; otherwise a valid compacted temp is promoted, or a valid backup is restored.
   - If no candidate validates, all artifacts are preserved and startup returns an error.
   - Compaction now emits committed chunks of at most 10,000 keys. Empty compaction produces a valid header-only database. The next transaction ID is preserved correctly.
   - Compaction writes use a complete-write helper and preflight the actual number of transaction envelopes.

5. **Project test/build cleanup**
   - Removed duplicate Go source/test copies from `public/` that caused `go test ./...` to fail. Canonical CLI source remains under `internal/cli/`; the benchmark remains under `tests/`.
   - The 1M-key performance SLA test is skipped under `-race`; race instrumentation is not a meaningful environment for a 2-second performance threshold.
   - Updated architecture, breaking-change, demo, and README documentation to match the implemented format and specified process-kill model. `PROMPTS.md` was left unchanged as a chronological prompt record.

## Verification performed in this review environment

| Check | Result |
|---|---|
| `go test ./...` | PASS |
| `go test -race ./...` | PASS |
| `go vet ./...` | PASS |
| Windows amd64 cross-compile of all Go test packages | PASS |
| Windows amd64 CLI build | PASS |
| Python syntax compilation | PASS |
| Python torture oracle self-tests | 4/4 PASS |
| Mixed process-kill torture | 500 rounds, seed 42, 0 invariant violations |
| Large-values torture | 30 rounds, seed 42, 0 invariant violations |
| Large-batch torture | 30 rounds, seed 42, 0 invariant violations |
| Recovery-crash torture | 30 rounds, seed 42, 0 invariant violations |
| Compaction-crash torture | 30 rounds, seed 42, 0 invariant violations |
| Chaos torture | 30 rounds, seed 42, 0 invariant violations |
| 1,000,000-key recovery benchmark | PASS; 316.23 ms in this review environment; 41 MB generated WAL |
| Empty-database compaction/reopen | PASS |
| 10,001-key chunked compaction/reopen | PASS |
| Cross-process lock regression test | PASS (same-host second-open contention test) |

## Important scope and remaining validation

- The Windows implementation was cross-compiled successfully, but this review environment is Linux. Run the full suite and compaction/process-lock tests on the target Windows machine before submission; cross-compilation is not a substitute for Windows runtime testing.
- The recovery benchmark result is hardware- and environment-dependent. Report the exact result from the machine used for the live demo.
- The database is intentionally **single-process**. A second process is rejected while the database lock is held; this is not a multi-process database.
- The required failure model is abrupt process termination while the OS and storage device remain operational. No guarantee is made for power loss, OS crash, dishonest hardware, or arbitrary bit rot.
- The React dashboard still contains sample/demo state; it should not be described as live database telemetry unless separately connected to the Go engine.
