# TORQ Architecture & Crash Invariant Specification
**System**: TORQ ("Kill It and Prove It" 24-Hour Hackathon)  
**Authors**: Tarun & AI Implementation Partner  
**Target Invariant**: Correctness under abrupt process termination (`kill -9`, Win32 `TerminateProcess`). The stated model assumes the OS and storage device remain operational; power-loss durability is not claimed.  
**Dependencies**: Pure Go Standard Library & Python 3 Standard Library only. Zero third-party packages.

---

## 1. System Overview

TORQ is an embedded key-value database engine paired with an independent correctness verification oracle. It is designed from first principles to guarantee ACID durability, transactional atomicity, and recovery for the specified abrupt-process-termination crash model.

```
+-------------------------------------------------------------------------+
|                               TORQ Engine                               |
|                                                                         |
|  +-----------------------+     +-------------------------------------+  |
|  | CLI / Protocol Engine |     |         Concurrency Controller      |  |
|  | (open, put, get, del, |     |           (sync.RWMutex)            |  |
|  | batch, dump, compact) |     +-------------------------------------+  |
|  +-----------+-----------+                        |                     |
|              |                                    v                     |
|              v                         +---------------------+          |
|  +-----------------------+             |   In-Memory Index   |          |
|  | Two-Phase Sync Engine |<----------->|  key -> (off, len)  |          |
|  +-----------+-----------+             +---------------------+          |
|              |                                                          |
|              v                                                          |
|  +-------------------------------------------------------------------+  |
|  |             Write-Ahead Log (WAL) & Recovery Subsystem             |  |
|  |       [File Header] [BEGIN] [PUT/DEL Ops] [SYNC] [COMMIT] [SYNC]  |  |
|  +-----------------------------------+-------------------------------+  |
+--------------------------------------|----------------------------------+
                                       |
                           (ReadAt on demand / Append)
                                       v
                     +-----------------------------------+
                     |        Storage Layer (Disk)       |
                     | main.db | .compact.tmp | .compact.bak | .lock |
                     +-----------------------------------+
```

### Core Architecture Characteristics
0. **Single-Process Ownership**: `db.Open()` acquires an OS-level exclusive lock on a stable `<dbPath>.lock` sidecar before resolving compaction artifacts or opening the data file. The lock is released on `Close()` or automatically by the OS when the process exits. The in-process `sync.RWMutex` does not substitute for this cross-process lock.
1. **Values Stay on Disk**: Values are not retained in the index. The in-memory hash index stores each key string and a compact `(ValueOffset, ValueLength)` entry. Lookups issue a single `file.ReadAt` call under a read lock.
2. **Commit-Gated Index Updates**: Operations within a transaction (`Put`, `Delete`, `Batch`) never touch the in-memory index until their `COMMIT` record has been successfully written and synced to persistent storage.
3. **Sequential Buffered Streaming**: Recovery uses a reusable 1 MB buffered reader and stages at most one transaction before applying it to the index. The one-million-key recovery benchmark is measured separately from race-instrumented tests.
4. **Independent Python Oracle**: `torture/torture.py` maintains an independent mathematical state machine, continuously firing random-window SIGKILL termination signals while asserting all 5 core invariants.

---

## 2. Frozen On-Disk Binary Format (v1)

All integers are stored in **Little-Endian** byte order. All records are framed with magic markers and protected by standard IEEE CRC-32 checksums.

### 2.1 File Header (Fixed 16 Bytes)
Every valid TORQ database file begins with a 16-byte header written and synced on creation.

```text
Offset  Size  Field
0       4     Magic = "TORQ"
4       2     Format version = 1 (uint16 LE)
6       2     Header size = 16 (uint16 LE)
8       4     Reserved = 0 (uint32 LE)
12      4     CRC32 IEEE over bytes 0..11
```
- **Magic**: `TORQ` (4 bytes).
- **Version**: `1` (uint16 little-endian).
- **Header size**: `16` (uint16 little-endian).
- **Reserved**: `0` (uint32 little-endian).
- **Header CRC**: IEEE CRC32 over bytes 0..11.

### 2.2 Record Envelope Format
Every record in the WAL uses a standard 18-byte envelope (14-byte header + 4-byte CRC32 trailer):

```
+--------------------+---------------------------------------------------+
| Field              | Size / Encoding                                   |
+--------------------+---------------------------------------------------+
| Marker             | 1 byte: 0xA5 (RecordMarker)                       |
| RecordType         | 1 byte: 0x01 (BEGIN), 0x02 (PUT),                 |
|                    |         0x03 (DEL),   0x04 (COMMIT)               |
| TxnID              | 8 bytes: uint64 Little-Endian                     |
| BodyLength         | 4 bytes: uint32 Little-Endian                     |
| Payload Body       | BodyLength bytes (variable)                       |
| CRC-32 Checksum    | 4 bytes: uint32 Little-Endian (IEEE table)        |
+--------------------+---------------------------------------------------+
```
*CRC Scope*: The 4-byte CRC-32 is calculated over all preceding bytes in the record: `Marker + RecordType + TxnID + BodyLength + PayloadBody`.

### 2.3 Record Types and Body Encodings

#### Type 0x01: BEGIN
Marks the start of an atomic transaction.
- **BodyLength**: Fixed 4 bytes
- **Body**: `OpCount (uint32 LE)` — the exact count of operations expected in this transaction.
- **Total Record Size**: $14 + 4 + 4 = 22\text{ bytes}$.

#### Type 0x02: PUT
Contains a key-value assignment within an active transaction.
- **Body Layout**:
  - `KeyLength (uint16 LE)`: 2 bytes (1 to 1,024 bytes)
  - `ValueLength (uint32 LE)`: 4 bytes (0 to 1,048,576 bytes)
  - `KeyBytes`: `KeyLength` bytes
  - `ValueBytes`: `ValueLength` bytes
- **Value Offset Calculation**: `ValueOffset = RecordOffset + 14 (header) + 2 (keyLen) + 4 (valLen) + keyLen`.
- **Total Record Size**: $24 + \text{KeyLength} + \text{ValueLength}\text{ bytes}$.

#### Type 0x03: DELETE
Marks a key deletion within an active transaction.
- **Body Layout**:
  - `KeyLength (uint16 LE)`: 2 bytes (1 to 1,024 bytes)
  - `KeyBytes`: `KeyLength` bytes
- **Total Record Size**: $20 + \text{KeyLength}\text{ bytes}$.

#### Type 0x04: COMMIT
Marks the successful atomic completion of a transaction.
- **BodyLength**: Fixed 4 bytes
- **Body**: `OpCount (uint32 LE)` — must match the `OpCount` specified in `BEGIN` and the actual operations counted.
- **Total Record Size**: $14 + 4 + 4 = 22\text{ bytes}$.

---

## 3. Two-Phase Write & Sync Protocol

To eliminate torn batches and guarantee that a client acknowledgement reflects permanent disk durability, every write transaction (`Put`, `Delete`, `Batch`) follows a strict two-phase synchronization protocol:

```
Caller (Put / Delete / Batch)
   |
   |-- 1. Hold Exclusive Lock (db.mu.Lock())
   |-- 2. Preflight Disk Limit Check (limits.CheckFit)
   |-- 3. Failpoint: before_write
   |-- 4. Stream BEGIN Record (OpCount = N)
   |-- 5. Stream N Operation Records (PUT / DELETE)
   |-- 6. Failpoint: after_ops_before_sync
   |-- 7. Phase 1 Sync: file.Sync() [Ensures all payloads are durable]
   |-- 8. Failpoint: after_ops_after_sync
   |-- 9. Stream COMMIT Record
   |-- 10. Failpoint: after_commit_before_sync
   |-- 11. Phase 2 Sync: file.Sync() [Ensures commit marker is durable]
   |-- 12. Update In-Memory Index (idx.ApplyCommittedOps)
   |-- 13. Failpoint: after_commit_sync_before_ack
   |-- 14. Advance nextTxnID & Unlock
   |
   v
Return nil (Client ACK)
```

### Why Two Syncs?
If a single `Sync()` were called only after writing `COMMIT`, a crash could allow a drive controller to reorder writes such that the `COMMIT` record was written to magnetic/flash media while prior `PUT` body records were still in volatile drive cache. On recovery, the scanner would find a valid `COMMIT` marker pointing to corrupt or missing data records. The two-phase protocol orders payload synchronization before the commit record is written. The benchmark and guarantee are scoped to the problem statement's process-kill model, not sudden power loss or dishonest storage hardware.

---

## 4. Recovery Subsystem (`recovery.Recover`)

Recovery runs unconditionally every time `db.Open()` is called. It replays committed transactions, repairs an interrupted append at EOF, and fails closed on detected middle-file corruption.

### Recovery Algorithm
1. **Header Validation**: Checks the 16-byte header: `TORQ` magic, version, header size, reserved field, and CRC32.
2. **Sequential Buffered Scan**: Reads sequentially from byte offset 16 using a reusable 1 MB buffered reader.
3. **Transactional Isolation via Staging**:
   - Encountering `BEGIN`: Sets `inTxn = true`, resets `actualOpCount = 0`, allocates `inFlightOps`.
   - Encountering `PUT` / `DEL`: Appends `index.Op` to `inFlightOps`.
   - Encountering `COMMIT`: Verifies `rec.OpCount == actualOpCount`. Applies `inFlightOps` atomically to the in-memory index; advances `lastCommittedOffset = loc.Offset + loc.TotalLength`; updates `maxCommittedTxnID`.
4. **Torn Tail Truncation**:
   - An incomplete final record, an uncommitted final transaction, or a malformed/checksum-invalid frame that demonstrably occupies the physical EOF tail is treated as an interrupted append and truncated to `lastCommittedOffset`, followed by `file.Sync()`.
   - A malformed transaction sequence or corruption before later bytes is reported as corruption and is not silently truncated.
   - Next transaction ID is initialized to `maxCommittedTxnID + 1`.

---

## 5. Crash-Safe Compaction & The C1–C5 Swap Matrix

Compaction creates a clean, minimal WAL containing only the latest version of live keys, purging tombstones and overwritten historical records.

### Compaction Execution
1. Take Database Read Lock (`db.mu.RLock()`).
2. Snapshot all live keys from the in-memory index.
3. Open temporary file: `main.db.compact.tmp`.
4. Write the 16-byte `TORQ` v1 header with CRC32.
5. Stream each live key as a single atomic transaction:
   - Read value from `main.db` via `file.ReadAt`.
   - Write `BEGIN`, `PUT`, `COMMIT` records into `main.db.compact.tmp`.
6. Flush and sync: `tmpFile.Sync()`.
7. Release read lock.
8. Take exclusive lock (`db.mu.Lock()`).
9. Close active file descriptor to release file handles (essential on Windows).
9. Three-step file rename:
    - Step A: `main.db` $\to$ `main.db.compact.bak`
    - Step B: `main.db.compact.tmp` $\to$ `main.db`
    - Step C: Remove `main.db.compact.bak`
10. Reopen `main.db` in `db.DB` and update index offsets.

### Startup Resolution Matrix (C1–C5)
When `db.Open(path)` runs, before accessing `main.db`, `ResolveStartupResolution(path)` inspects the directory for lingering `.tmp` and `.bak` files:

```
+----+-------------+------------+------------+-------------------------------------------+
| ID | main.db     | .tmp file  | .bak file  | Recovery Resolution Action                |
+----+-------------+------------+------------+-------------------------------------------+
| C1 | Valid       | Incomplete | None       | Discard .tmp; open main.db                |
| C2 | Valid       | Valid      | None       | Discard .tmp; open main.db                |
| C3 | Missing     | Valid      | Valid      | Rename .tmp -> main.db; remove .bak       |
| C4 | Valid (new) | None       | Valid      | Remove .bak; open main.db                 |
| C5 | Corrupt     | Valid      | None       | Promote .tmp -> main.db                   |
+----+-------------+------------+------------+-------------------------------------------+
```

---

## 6. Complete 32-Scenario Crash Matrix

The system explicitly defines, verifies, and tests 32 discrete crash scenarios across 6 operational groups.

| ID | Scenario Name | Failpoint / Trigger | Crash Injection Point | Post-Crash Invariant & State | Verifying Test |
|---|---|---|---|---|---|
| **A1** | Crash Before Single Write | `before_write` | Before any bytes hit disk buffer | 0 bytes written; key not found; file identical | `internal/recovery.TestFailpoints` |
| **A2** | Crash Mid Single Write | `mid_write` | After partial record header written | Torn record truncated; key not found | `internal/recovery.TestFailpoints` |
| **A3** | Crash After Ops Before Sync | `after_ops_before_sync` | Data in OS buffer, un-synced | Uncommitted transaction discarded | `internal/recovery.TestFailpoints` |
| **A4** | Crash After Ops After Sync | `after_ops_after_sync` | Ops durable, COMMIT unwritten | Uncommitted transaction truncated at tail | `torture/torture.py` (mixed) |
| **A5** | Crash Mid Commit Write | `mid_commit` | Partial COMMIT record on disk | CRC mismatch on COMMIT; txn discarded | `torture/torture.py` (mixed) |
| **A6** | Crash After Commit Before Sync | `after_commit_before_sync` | COMMIT written, un-synced | If synced: durable; If not: truncated | `torture/torture.py` (mixed) |
| **A7** | Crash After Commit Sync Before Ack | `after_commit_sync_before_ack` | COMMIT synced, before client return | Transaction is durable and recoverable | `internal/recovery.TestFailpoints` |
| **B1** | Crash Mid Batch Write | `batch_mid_write` | Part of batch ops appended | All ops in batch discarded; 0 keys updated | `internal/recovery.TestFailpoints` |
| **B2** | Crash After All Batch Ops Before Commit | `batch_after_ops` | 100% ops appended, 0 COMMIT | Batch completely discarded; atomic | `torture/torture.py` (large-batch) |
| **B3** | Crash After Batch Commit Before Sync | `batch_after_commit_before_sync` | COMMIT appended, un-synced | All-or-nothing: either 100% or 0% durable | `internal/recovery.TestFailpoints` |
| **B4** | Crash After Batch Sync Before Ack | `batch_after_commit_sync_before_ack`| COMMIT synced, client un-acked | Entire batch durable and readable | `internal/recovery.TestFailpoints` |
| **C1** | Crash Compaction Before Sync | `compaction_before_sync` | During `.tmp` file writing | `.tmp` discarded; original `main.db` intact | `internal/compaction.TestCompaction` |
| **C2** | Crash After Sync Before Rename | `compaction_after_sync_before_rename`| `.tmp` fully synced | `.tmp` discarded; `main.db` remains source | `internal/compaction.TestCompaction` |
| **C3** | Crash After Rename Main to Bak | `compaction_after_rename_main_to_bak`| `main.db.bak` and `.tmp` exist | Startup promotes `.tmp` $\to$ `main`; removes bak | `internal/compaction.TestCompaction` |
| **C4** | Crash After Rename Tmp to Main | `compaction_after_rename_tmp_to_main`| `main.db` (new) and `.bak` exist | Startup removes `.bak`; opens `main.db` | `internal/compaction.TestCompaction` |
| **C5** | Crash During Reopening Compacted DB | `compaction_reopen` | After swap complete | Startup opens newly compacted `main.db` | `torture/torture.py` (compact-crash) |
| **D1** | Two Writers Different Keys | Concurrent threads | Concurrent `Put(k1)` and `Put(k2)` | Serialized by mutex; both durable | `internal/db.TestScenarios_D1_to_D4` |
| **D2** | Two Writers Same Key | Concurrent threads | Concurrent `Put(k, v1)` and `Put(k, v2)` | Serialized; last write wins; no torn values | `internal/db.TestScenarios_D1_to_D4` |
| **D3** | Long Batch + Single Puts | Concurrent threads | 100-op batch vs continuous `Put` | Single puts interleave cleanly around batch | `internal/db.TestScenarios_D1_to_D4` |
| **D4** | Reader During Batch Write | Concurrent threads | `Get()` called while batch appending | Reader sees either old state or new, never partial | `internal/db.TestScenarios_D1_to_D4` |
| **E1** | Disk Full Single Write | `limits.ErrDiskFull` | Preflight size check rejects write | File byte-for-byte identical; 0 bytes written | `tests.TestDiskFull_E1` |
| **E2** | Disk Full Batch Write | `limits.ErrDiskFull` | Preflight check on total batch size | File byte-for-byte identical; batch rejected | `tests.TestDiskFull_E2` |
| **E3** | Read Operations on Full Disk | Capacity exhausted | Read operations (`Get`, `KeyCount`, etc.) | Read-only ops succeed with 0 disk writes | `tests.TestDiskFull_E3` |
| **E4** | Recovery on Full Disk | Capacity exhausted | `Open()` called on 100% full DB | Recovery completes without spare bytes | `tests.TestDiskFull_E4` |
| **E5** | Compaction Restores Write Capability | Capacity exhausted | `Compact()` called on full DB | Purges dead records; file shrinks; writes resume | `tests.TestDiskFull_E5` |
| **F1** | Crash Mid Recovery Scan | `recovery_mid_scan` | During sequential scan of WAL | Read-only scan; file completely untouched | `tests.TestCrashDuringRecovery_F1_to_F3` |
| **F2** | Crash Before Truncating Tail | `recovery_before_truncate` | After finding bad tail, before truncate | Next `Open()` detects bad tail and truncates | `tests.TestCrashDuringRecovery_F1_to_F3` |
| **F3** | Crash After Truncating Tail | `recovery_after_truncate` | Truncate done, before return | File already clean; next `Open()` succeeds | `tests.TestCrashDuringRecovery_F1_to_F3` |
| **F4** | Recovery on 1-Byte Truncation | Truncate file by 1 byte | Last byte of CRC missing | Scanner truncates damaged record to last commit | `internal/recovery.TestRecovery_F4` |

---

## 7. Independent Torture Harness & Mathematical Oracle

The correctness oracle in `torture/torture.py` operates without accessing internal database variables. It asserts the **5 Universal Crash-Recovery Invariants**:

```
                    +-----------------------------+
                    |  Independent Torture Oracle |
                    |      (Python 3 Stdlib)      |
                    +--------------+--------------+
                                   |
             +---------------------+---------------------+
             |                                           |
             v                                           v
+--------------------------+               +--------------------------+
|  Base State Tracker      |               |  In-Flight Op Ledger     |
|  (Verified durable keys) |               |  (Acked vs Un-Acked Ops) |
+--------------------------+               +--------------------------+
             |                                           |
             +---------------------+---------------------+
                                   |
                                   v
             [Process Killed via SIGKILL / TerminateProcess]
                                   |
                                   v
             +-------------------------------------------+
             |      Post-Crash Invariant Verification    |
             |                                           |
             |  1. Uncommitted Txns Never Survive        |
             |  2. Acked Committed Txns Never Lost       |
             |  3. Values Bit-for-Bit Verified           |
             |  4. Multi-Op Batches Never Torn           |
             |  5. kvdb open Always Self-Heals           |
             +-------------------------------------------+
```

### Self-Test Modes
The oracle contains 4 built-in defect-injection tests executed via `python3 torture/torture.py --self-test` to prove it never produces false negatives:
1. **Broken Engine 1 (Lost Commit)**: Drops an acknowledged key; caught by Oracle.
2. **Broken Engine 2 (Corrupted Value)**: Flips 1 byte in a value; caught by Oracle.
3. **Broken Engine 3 (Torn Batch)**: Commits 1 op of a 2-op batch; caught by Oracle.
4. **Broken Engine 4 (Phantom Key)**: Injects an unwritten key; caught by Oracle.
