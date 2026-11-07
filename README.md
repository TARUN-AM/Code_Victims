# TORQ
**Crash-Safe, Embedded Key-Value Database & Torture Test Harness**  
*Built from scratch for the 24-hour "Kill It and Prove It" Hackathon.*

---

## 1. Overview
TORQ is a process-kill-resilient embedded key-value database written in Go (standard library only) paired with an independent torture tool written in Python 3 (standard library only).

### The Invariant (after any process kill and after `open()` recovery completes):
1. Every operation that returned success before the kill is present and durable.
2. Every operation that had not returned success is fully present or fully absent.
3. A batch is strictly atomic: never partially present.
4. No key ever holds a value that was never written to it (no torn values, no cross-key leakage).
5. `open()` recovers interrupted append tails and compaction swaps after process termination; detected middle-file corruption is reported rather than silently discarded.

---

## 2. On-Disk Binary Format (Frozen v1)

All fields are stored in **little-endian** byte order. Every record ends with an IEEE CRC-32 checksum (polynomial `0xEDB88320`) covering every byte from offset 0 of the record up to the start of the CRC.

### File Header (16 Bytes Fixed at Offset 0)
| Offset | Field | Type / Width | Expected Value | Description |
|---|---|---|---|---|
| `0..3` | `Magic` | `[4]byte` (4 B) | `"TORQ"` (`0x54, 0x4F, 0x52, 0x51`) | File type identifier |
| `4..5` | `Version` | `uint16` (2 B) | `1` (`0x0001`) | Format version |
| `6..7` | `HeaderSize`| `uint16` (2 B) | `16` (`0x0010`) | Header size in bytes |
| `8..11`| `Reserved` | `uint32` (4 B) | `0` (`0x00000000`) | Padding for 64-bit alignment |
| `12..15`| `HeaderCRC`| `uint32` (4 B) | `CRC32(bytes 0..11)` | IEEE CRC-32 covering header bytes 0..11 |

### Record Framing (All Records)
Each record begins with a fixed 14-byte header and ends with a 4-byte CRC32 trailer (total overhead = 18 bytes).

| Offset | Field | Type / Width | Description |
|---|---|---|---|
| `0` | `Marker` | `uint8` (1 B) | Synchronization byte: always `0xA5` |
| `1` | `Type` | `uint8` (1 B) | Record type: `1`=BEGIN, `2`=PUT, `3`=DELETE, `4`=COMMIT |
| `2..9` | `TxnID` | `uint64` (8 B) | Monotonically increasing transaction ID |
| `10..13` | `BodyLen` | `uint32` (4 B) | Exact byte length of the body payload |
| `14..14+BodyLen-1` | `Body` | `[]byte` | Record body payload (format depends on `Type`) |
| `End-4..End-1` | `RecordCRC` | `uint32` (4 B) | IEEE CRC-32 covering offsets `0 .. 13+BodyLen` |

### Record Body Payloads

#### 1. BEGIN Record (`Type = 1`)
- Length: 4 bytes
- Fields:
  - `0..3`: `OpCount` (`uint32`, 4 B): Count of operations in this transaction (1 for single put/delete; up to 10,000 for batch).

#### 2. PUT Record (`Type = 2`)
- Length: `6 + KeyLen + ValueLen` bytes
- Fields:
  - `0..1`: `KeyLen` (`uint16`, 2 B): Key byte length (`1 .. 1,024`)
  - `2..5`: `ValueLen` (`uint32`, 4 B): Value byte length (`0 .. 1,048,576`)
  - `6 .. 6+KeyLen-1`: `Key` (`[]byte`): Key bytes
  - `6+KeyLen .. End`: `Value` (`[]byte`): Value bytes

#### 3. DELETE Record (`Type = 3`)
- Length: `2 + KeyLen` bytes
- Fields:
  - `0..1`: `KeyLen` (`uint16`, 2 B): Key byte length (`1 .. 1,024`)
  - `2 .. End`: `Key` (`[]byte`): Key bytes

#### 4. COMMIT Record (`Type = 4`)
- Length: 4 bytes
- Fields:
  - `0..3`: `OpCount` (`uint32`, 4 B): Must match `OpCount` in preceding BEGIN record.

---

## 3. System Limits
- Key Length: 1 .. 1,024 bytes
- Value Length: 0 .. 1,048,576 bytes (1 MB)
- Max Keys: At least 1,000,000 keys
- Batch Size: 1 .. 10,000 operations
- Maximum Body Length Preflight Cap: 1,049,606 bytes (rejects corrupted lengths before memory allocation)

---

## 4. Concurrency and Process Ownership
- Multiple reader goroutines may hold the database read lock concurrently.
- Writer goroutines are supported, but each transaction is serialized under the exclusive database lock so WAL records cannot interleave.
- A stable `<database-path>.lock` sidecar is held with an OS-level exclusive lock. A second process cannot open the same database while the first owns it. The lock is released by `Close()` or automatically when the process exits.
- This is a single-process database, not a multi-process or parallel-WAL-writer engine.

## 5. Build & Test Commands
```bash
# Build binary
go build -o bin/kvdb ./cmd/kvdb

# Run unit tests with verbose output
go test ./... -v

# Run race detector (the 1M performance SLA test is skipped under -race)
go test -race ./...

# Run 1,000,000-key recovery SLA test without race instrumentation
go test ./tests -run TestBenchmark_1M_Keys_Recovery -v -count=1
```
