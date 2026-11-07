package wal

import (
	"hash/crc32"
)

// CalculateChecksum returns the IEEE CRC-32 checksum of data using Go hardware assembly.
func CalculateChecksum(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

// CalculateChecksumCombined updates an existing CRC32 with new bytes.
func CalculateChecksumCombined(crc uint32, data []byte) uint32 {
	return crc32.Update(crc, crc32.IEEETable, data)
}

// VerifyChecksum validates that data has the expected IEEE CRC-32 checksum.
func VerifyChecksum(data []byte, expected uint32) bool {
	return crc32.ChecksumIEEE(data) == expected
}
