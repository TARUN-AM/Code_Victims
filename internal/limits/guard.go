package limits

import (
	"errors"
	"math"
	"github.com/TARUN-AM/torq/internal/wal"
)

// ErrDiskFull is returned when an operation or batch would exceed the configured MaxBytes limit.
var ErrDiskFull = errors.New("disk full: write would exceed max_bytes limit")

// ValidateKey checks that a key satisfies system limits (1..1024 bytes).
func ValidateKey(key []byte) error {
	l := len(key)
	if l < wal.MinKeyLength {
		return wal.ErrKeyEmpty
	}
	if l > wal.MaxKeyLength {
		return wal.ErrKeyTooLarge
	}
	return nil
}

// ValidateValue checks that a value satisfies system limits (0..1,048,576 bytes).
func ValidateValue(value []byte) error {
	if len(value) > wal.MaxValueLength {
		return wal.ErrValueTooLarge
	}
	return nil
}

// ValidateBatchSize checks that a batch op count satisfies limits (1..10,000 ops).
func ValidateBatchSize(count int) error {
	if count < wal.MinBatchSize {
		return wal.ErrBatchEmpty
	}
	if count > wal.MaxBatchSize {
		return wal.ErrBatchTooLarge
	}
	return nil
}

// EncodedTxnOverhead returns the fixed byte size for BEGIN and COMMIT records.
// BEGIN: 14 (header) + 4 (opCount) + 4 (crc) = 22 bytes
// COMMIT: 14 (header) + 4 (opCount) + 4 (crc) = 22 bytes
// Total = 44 bytes
const EncodedTxnOverhead int64 = 44

// EncodedPutSize calculates the exact encoded size of a PUT record in the WAL.
// 14 (header) + 2 (keyLen) + 4 (valLen) + len(k) + len(v) + 4 (crc) = 24 + len(k) + len(v)
func EncodedPutSize(keyLen int, valLen int) int64 {
	return int64(wal.RecordEnvelopeSize + 6 + keyLen + valLen)
}

// EncodedDeleteSize calculates the exact encoded size of a DELETE record in the WAL.
// 14 (header) + 2 (keyLen) + len(k) + 4 (crc) = 20 + len(k)
func EncodedDeleteSize(keyLen int) int64 {
	return int64(wal.RecordEnvelopeSize + 2 + keyLen)
}

// CheckFit verifies whether adding incomingBytes to currentSize will exceed maxBytes.
// Uses overflow-safe arithmetic. If maxBytes is 0, disk capacity is unlimited.
func CheckFit(currentSize int64, incomingBytes int64, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}

	if incomingBytes < 0 || currentSize < 0 {
		return ErrDiskFull
	}

	// Overflow guard
	if incomingBytes > math.MaxInt64-currentSize {
		return ErrDiskFull
	}

	if currentSize+incomingBytes > maxBytes {
		return ErrDiskFull
	}

	return nil
}
