package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// RecordLocation records the exact file offsets for a decoded record on disk.
type RecordLocation struct {
	Offset      int64  // File offset where the record begins (at 0xA5 marker)
	TotalLength int    // Total byte length of record including header and CRC
	ValueOffset int64  // Exact file offset of the value payload (for PUT records)
	ValueLength uint32 // Byte length of the value payload (for PUT records)
}

// Reader sequentially scans WAL records from an io.Reader while tracking exact offsets.
type Reader struct {
	r          *bufio.Reader
	offset     int64
	headerBuf  [RecordHeaderSize]byte
	trailerBuf [RecordTrailerSize]byte
	bodyBuf    []byte
}

// NewReader creates a new WAL reader starting at the specified file offset.
func NewReader(r io.Reader, startOffset int64) *Reader {
	return &Reader{
		r:       bufio.NewReaderSize(r, 1024*1024), // 1 MB buffer for maximum sequential scanning throughput
		offset:  startOffset,
		bodyBuf: make([]byte, 4096),
	}
}

// Offset returns the current file offset in the stream.
func (wr *Reader) Offset() int64 {
	return wr.offset
}

// Next reads and validates the next record from the stream.
// Returns:
//   - rec: the decoded record (or nil if EOF)
//   - loc: offset and length metadata for indexing
//   - err: nil on success, io.EOF if clean end-of-file, ErrIncompleteRecord if truncated, or ErrInvalidRecord
func (wr *Reader) Next() (*Record, *RecordLocation, error) {
	startOffset := wr.offset

	// 1. Read record header (14 bytes)
	_, err := io.ReadFull(wr.r, wr.headerBuf[:])
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil, ErrIncompleteRecord
		}
		return nil, nil, err
	}

	// 2. Validate frame marker
	if wr.headerBuf[0] != RecordMarker {
		return nil, nil, fmt.Errorf("%w: marker 0x%02X at offset %d", ErrInvalidRecord, wr.headerBuf[0], startOffset)
	}

	recType := wr.headerBuf[1]
	if recType < RecordTypeBegin || recType > RecordTypeCommit {
		return nil, nil, fmt.Errorf("%w: invalid type %d at offset %d", ErrInvalidRecord, recType, startOffset)
	}

	txnID := binary.LittleEndian.Uint64(wr.headerBuf[2:10])
	bodyLen := binary.LittleEndian.Uint32(wr.headerBuf[10:14])

	// Strict preflight bounds check on bodyLen before allocation
	maxAllowedBody := uint32(2 + 4 + MaxKeyLength + MaxValueLength)
	if bodyLen > maxAllowedBody {
		return nil, nil, fmt.Errorf("%w: body length %d exceeds cap %d at offset %d", ErrInvalidRecord, bodyLen, maxAllowedBody, startOffset)
	}

	// 3. Read body bytes using reusable buffer
	if cap(wr.bodyBuf) < int(bodyLen) {
		wr.bodyBuf = make([]byte, bodyLen)
	}
	bodyBuf := wr.bodyBuf[:bodyLen]
	if _, err := io.ReadFull(wr.r, bodyBuf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil, ErrIncompleteRecord
		}
		return nil, nil, err
	}

	// 4. Read CRC32 trailer (4 bytes)
	if _, err := io.ReadFull(wr.r, wr.trailerBuf[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil, ErrIncompleteRecord
		}
		return nil, nil, err
	}

	// 5. Verify CRC32 over header + body
	expectedCRC := binary.LittleEndian.Uint32(wr.trailerBuf[:])
	actualCRC := CalculateChecksum(wr.headerBuf[:])
	actualCRC = updateCRC(actualCRC, bodyBuf)

	if actualCRC != expectedCRC {
		return nil, nil, fmt.Errorf("%w: CRC mismatch at offset %d (expected %08x, got %08x)", ErrInvalidRecord, startOffset, expectedCRC, actualCRC)
	}

	// 6. Parse payload
	totalLen := RecordEnvelopeSize + int(bodyLen)
	loc := &RecordLocation{
		Offset:      startOffset,
		TotalLength: totalLen,
	}

	rec := &Record{
		Type:  recType,
		TxnID: txnID,
	}

	switch recType {
	case RecordTypeBegin, RecordTypeCommit:
		if len(bodyBuf) != 4 {
			return nil, nil, fmt.Errorf("%w: invalid op_count body size %d", ErrInvalidRecord, len(bodyBuf))
		}
		rec.OpCount = binary.LittleEndian.Uint32(bodyBuf[0:4])

	case RecordTypePut:
		if len(bodyBuf) < 6 {
			return nil, nil, fmt.Errorf("%w: put body too short (%d < 6)", ErrInvalidRecord, len(bodyBuf))
		}
		keyLen := int(binary.LittleEndian.Uint16(bodyBuf[0:2]))
		valLen := binary.LittleEndian.Uint32(bodyBuf[2:6])

		if keyLen < MinKeyLength || keyLen > MaxKeyLength {
			return nil, nil, fmt.Errorf("%w: invalid key length %d", ErrInvalidRecord, keyLen)
		}
		if valLen > MaxValueLength {
			return nil, nil, fmt.Errorf("%w: invalid value length %d", ErrInvalidRecord, valLen)
		}
		if len(bodyBuf) != 6+keyLen+int(valLen) {
			return nil, nil, fmt.Errorf("%w: put body length mismatch: expected %d, got %d", ErrInvalidRecord, 6+keyLen+int(valLen), len(bodyBuf))
		}

		rec.Key = bodyBuf[6 : 6+keyLen]
		rec.Value = bodyBuf[6+keyLen:]

		// Value file offset is: startOffset + 14 (header) + 2 (keyLen) + 4 (valLen) + keyLen
		loc.ValueOffset = startOffset + int64(RecordHeaderSize+6+keyLen)
		loc.ValueLength = valLen

	case RecordTypeDelete:
		if len(bodyBuf) < 2 {
			return nil, nil, fmt.Errorf("%w: delete body too short (%d < 2)", ErrInvalidRecord, len(bodyBuf))
		}
		keyLen := int(binary.LittleEndian.Uint16(bodyBuf[0:2]))
		if keyLen < MinKeyLength || keyLen > MaxKeyLength {
			return nil, nil, fmt.Errorf("%w: invalid delete key length %d", ErrInvalidRecord, keyLen)
		}
		if len(bodyBuf) != 2+keyLen {
			return nil, nil, fmt.Errorf("%w: delete body length mismatch: expected %d, got %d", ErrInvalidRecord, 2+keyLen, len(bodyBuf))
		}

		rec.Key = bodyBuf[2 : 2+keyLen]
	}

	wr.offset += int64(totalLen)
	return rec, loc, nil
}

// updateCRC computes rolling IEEE CRC32 using the standard table without memory allocations.
func updateCRC(crc uint32, data []byte) uint32 {
	return CalculateChecksumCombined(crc, data)
}
