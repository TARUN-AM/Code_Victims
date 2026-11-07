package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frozen Protocol Constants
const (
	// FileHeaderMagic is the 4-byte ASCII magic identifier for TORQ data files
	FileHeaderMagic = "TORQ"
	// FormatVersion is the frozen v1 database format version
	FormatVersion uint16 = 1
	// FileHeaderSize is the fixed size in bytes of the file header
	FileHeaderSize = 16
	// RecordMarker is the fixed synchronization byte preceding every record
	RecordMarker byte = 0xA5

	// Envelope sizes:
	// Header: Marker(1) + Type(1) + TxnID(8) + BodyLen(4) = 14 bytes
	RecordHeaderSize = 14
	// Trailer: CRC32(4) = 4 bytes
	RecordTrailerSize = 4
	// Total record envelope overhead = 18 bytes
	RecordEnvelopeSize = RecordHeaderSize + RecordTrailerSize

	// System Limits
	MinKeyLength   = 1
	MaxKeyLength   = 1024 // 1 KB
	MinValueLength = 0
	MaxValueLength = 1024 * 1024 // 1 MB = 1,048,576 bytes
	MinBatchSize   = 1
	MaxBatchSize   = 10000
)

// Record types
const (
	RecordTypeBegin  uint8 = 1
	RecordTypePut    uint8 = 2
	RecordTypeDelete uint8 = 3
	RecordTypeCommit uint8 = 4
)

// Standard Sentinel Errors
var (
	ErrIncompleteRecord = errors.New("incomplete record: unexpected EOF or truncated bytes")
	ErrInvalidRecord    = errors.New("invalid record: corrupt marker, checksum, or payload bounds")
	ErrCorruptHeader    = errors.New("corrupt file header: magic or checksum mismatch")
	ErrKeyTooLarge      = errors.New("key length exceeds maximum limit of 1024 bytes")
	ErrKeyEmpty         = errors.New("key cannot be empty")
	ErrValueTooLarge    = errors.New("value length exceeds maximum limit of 1 MB")
	ErrBatchTooLarge    = errors.New("batch exceeds maximum limit of 10000 operations")
	ErrBatchEmpty       = errors.New("batch must contain at least 1 operation")
)

// FileHeader represents the 16-byte fixed header at offset 0 of every TORQ file.
type FileHeader struct {
	Magic      [4]byte
	Version    uint16
	HeaderSize uint16
	Reserved   uint32
	CRC32      uint32
}

// Record represents a single WAL record in memory.
type Record struct {
	Type    uint8
	TxnID   uint64
	OpCount uint32 // Used for BEGIN and COMMIT records
	Key     []byte // Used for PUT and DELETE records
	Value   []byte // Used for PUT records
}

// EncodeFileHeader serializes the 16-byte file header into buf (must be at least 16 bytes).
func EncodeFileHeader(buf []byte) error {
	if len(buf) < FileHeaderSize {
		return fmt.Errorf("buffer too small for file header: %d < %d", len(buf), FileHeaderSize)
	}
	copy(buf[0:4], FileHeaderMagic)
	binary.LittleEndian.PutUint16(buf[4:6], FormatVersion)
	binary.LittleEndian.PutUint16(buf[6:8], FileHeaderSize)
	binary.LittleEndian.PutUint32(buf[8:12], 0) // Reserved
	crc := CalculateChecksum(buf[0:12])
	binary.LittleEndian.PutUint32(buf[12:16], crc)
	return nil
}

// DecodeFileHeader deserializes and verifies the 16-byte file header.
func DecodeFileHeader(buf []byte) (*FileHeader, error) {
	if len(buf) < FileHeaderSize {
		return nil, ErrIncompleteRecord
	}
	if string(buf[0:4]) != FileHeaderMagic {
		return nil, fmt.Errorf("%w: invalid magic %q", ErrCorruptHeader, string(buf[0:4]))
	}
	version := binary.LittleEndian.Uint16(buf[4:6])
	if version != FormatVersion {
		return nil, fmt.Errorf("%w: unsupported format version %d", ErrCorruptHeader, version)
	}
	headerSize := binary.LittleEndian.Uint16(buf[6:8])
	if headerSize != FileHeaderSize {
		return nil, fmt.Errorf("%w: unexpected header size %d", ErrCorruptHeader, headerSize)
	}
	expectedCRC := binary.LittleEndian.Uint32(buf[12:16])
	actualCRC := CalculateChecksum(buf[0:12])
	if actualCRC != expectedCRC {
		return nil, fmt.Errorf("%w: header CRC mismatch (expected %08x, got %08x)", ErrCorruptHeader, expectedCRC, actualCRC)
	}

	h := &FileHeader{
		Version:    version,
		HeaderSize: headerSize,
		Reserved:   binary.LittleEndian.Uint32(buf[8:12]),
		CRC32:      expectedCRC,
	}
	copy(h.Magic[:], buf[0:4])
	return h, nil
}

// EncodedRecordSize returns the total encoded byte count for a Record.
func EncodedRecordSize(rec *Record) (int, error) {
	bodyLen, err := EncodedBodySize(rec)
	if err != nil {
		return 0, err
	}
	return RecordEnvelopeSize + bodyLen, nil
}

// EncodedBodySize returns the byte count of the record body payload.
func EncodedBodySize(rec *Record) (int, error) {
	switch rec.Type {
	case RecordTypeBegin, RecordTypeCommit:
		return 4, nil // OpCount (uint32)
	case RecordTypePut:
		keyLen := len(rec.Key)
		valLen := len(rec.Value)
		if keyLen < MinKeyLength {
			return 0, ErrKeyEmpty
		}
		if keyLen > MaxKeyLength {
			return 0, ErrKeyTooLarge
		}
		if valLen > MaxValueLength {
			return 0, ErrValueTooLarge
		}
		// Body: KeyLen(2) + ValueLen(4) + Key(keyLen) + Value(valLen)
		return 2 + 4 + keyLen + valLen, nil
	case RecordTypeDelete:
		keyLen := len(rec.Key)
		if keyLen < MinKeyLength {
			return 0, ErrKeyEmpty
		}
		if keyLen > MaxKeyLength {
			return 0, ErrKeyTooLarge
		}
		// Body: KeyLen(2) + Key(keyLen)
		return 2 + keyLen, nil
	default:
		return 0, fmt.Errorf("%w: unknown record type %d", ErrInvalidRecord, rec.Type)
	}
}

// EncodeRecord serializes a Record into a new byte slice with its trailing CRC32.
func EncodeRecord(rec *Record) ([]byte, error) {
	bodyLen, err := EncodedBodySize(rec)
	if err != nil {
		return nil, err
	}
	totalLen := RecordEnvelopeSize + bodyLen
	buf := make([]byte, totalLen)
	if err := EncodeRecordToBuf(rec, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// EncodeRecordToBuf serializes a Record into an existing buffer of exact size.
func EncodeRecordToBuf(rec *Record, buf []byte) error {
	bodyLen, err := EncodedBodySize(rec)
	if err != nil {
		return err
	}
	totalLen := RecordEnvelopeSize + bodyLen
	if len(buf) < totalLen {
		return fmt.Errorf("buffer too small: %d < %d", len(buf), totalLen)
	}

	// 1. Header (14 bytes)
	buf[0] = RecordMarker
	buf[1] = rec.Type
	binary.LittleEndian.PutUint64(buf[2:10], rec.TxnID)
	binary.LittleEndian.PutUint32(buf[10:14], uint32(bodyLen))

	// 2. Body
	offset := 14
	switch rec.Type {
	case RecordTypeBegin, RecordTypeCommit:
		binary.LittleEndian.PutUint32(buf[offset:offset+4], rec.OpCount)
		offset += 4
	case RecordTypePut:
		keyLen := len(rec.Key)
		valLen := len(rec.Value)
		binary.LittleEndian.PutUint16(buf[offset:offset+2], uint16(keyLen))
		offset += 2
		binary.LittleEndian.PutUint32(buf[offset:offset+4], uint32(valLen))
		offset += 4
		copy(buf[offset:offset+keyLen], rec.Key)
		offset += keyLen
		copy(buf[offset:offset+valLen], rec.Value)
		offset += valLen
	case RecordTypeDelete:
		keyLen := len(rec.Key)
		binary.LittleEndian.PutUint16(buf[offset:offset+2], uint16(keyLen))
		offset += 2
		copy(buf[offset:offset+keyLen], rec.Key)
		offset += keyLen
	}

	// 3. Trailer: CRC32 covering offsets 0 .. offset-1
	crc := CalculateChecksum(buf[0:offset])
	binary.LittleEndian.PutUint32(buf[offset:offset+4], crc)
	return nil
}

// DecodeRecordFromBytes attempts to decode a single record from a raw byte slice.
// Returns:
//   - rec: the decoded Record
//   - bytesRead: total bytes consumed from buf
//   - err: nil on success, ErrIncompleteRecord if more bytes needed, or ErrInvalidRecord on corruption
func DecodeRecordFromBytes(buf []byte) (rec *Record, bytesRead int, err error) {
	if len(buf) < RecordHeaderSize {
		return nil, 0, ErrIncompleteRecord
	}

	// Validate Marker
	if buf[0] != RecordMarker {
		return nil, 0, fmt.Errorf("%w: expected marker 0x%02X, got 0x%02X", ErrInvalidRecord, RecordMarker, buf[0])
	}

	recType := buf[1]
	if recType < RecordTypeBegin || recType > RecordTypeCommit {
		return nil, 0, fmt.Errorf("%w: invalid record type %d", ErrInvalidRecord, recType)
	}

	txnID := binary.LittleEndian.Uint64(buf[2:10])
	bodyLen := binary.LittleEndian.Uint32(buf[10:14])

	// Guard against corrupt large bodyLen before allocating or calculating total length
	// Maximum possible body = 2 (KeyLen) + 4 (ValLen) + 1024 (Key) + 1048576 (Val) = 1,049,606 bytes
	maxAllowedBody := uint32(2 + 4 + MaxKeyLength + MaxValueLength)
	if bodyLen > maxAllowedBody {
		return nil, 0, fmt.Errorf("%w: body length %d exceeds maximum plausible body size %d", ErrInvalidRecord, bodyLen, maxAllowedBody)
	}

	totalLen := RecordEnvelopeSize + int(bodyLen)
	if len(buf) < totalLen {
		return nil, 0, ErrIncompleteRecord
	}

	// Verify CRC32
	expectedCRC := binary.LittleEndian.Uint32(buf[totalLen-RecordTrailerSize : totalLen])
	actualCRC := CalculateChecksum(buf[0 : totalLen-RecordTrailerSize])
	if actualCRC != expectedCRC {
		return nil, 0, fmt.Errorf("%w: CRC mismatch (expected %08x, got %08x)", ErrInvalidRecord, expectedCRC, actualCRC)
	}

	// Parse Body
	bodyBuf := buf[RecordHeaderSize : totalLen-RecordTrailerSize]
	r := &Record{
		Type:  recType,
		TxnID: txnID,
	}

	switch recType {
	case RecordTypeBegin, RecordTypeCommit:
		if len(bodyBuf) != 4 {
			return nil, 0, fmt.Errorf("%w: begin/commit body must be 4 bytes, got %d", ErrInvalidRecord, len(bodyBuf))
		}
		r.OpCount = binary.LittleEndian.Uint32(bodyBuf[0:4])

	case RecordTypePut:
		if len(bodyBuf) < 6 {
			return nil, 0, fmt.Errorf("%w: put body too short (%d < 6)", ErrInvalidRecord, len(bodyBuf))
		}
		keyLen := int(binary.LittleEndian.Uint16(bodyBuf[0:2]))
		valLen := int(binary.LittleEndian.Uint32(bodyBuf[2:6]))

		if keyLen < MinKeyLength || keyLen > MaxKeyLength {
			return nil, 0, fmt.Errorf("%w: invalid key length %d", ErrInvalidRecord, keyLen)
		}
		if valLen > MaxValueLength {
			return nil, 0, fmt.Errorf("%w: invalid value length %d", ErrInvalidRecord, valLen)
		}
		if len(bodyBuf) != 6+keyLen+valLen {
			return nil, 0, fmt.Errorf("%w: put body size mismatch: expected %d, got %d", ErrInvalidRecord, 6+keyLen+valLen, len(bodyBuf))
		}

		r.Key = make([]byte, keyLen)
		copy(r.Key, bodyBuf[6:6+keyLen])
		r.Value = make([]byte, valLen)
		copy(r.Value, bodyBuf[6+keyLen:6+keyLen+valLen])

	case RecordTypeDelete:
		if len(bodyBuf) < 2 {
			return nil, 0, fmt.Errorf("%w: delete body too short (%d < 2)", ErrInvalidRecord, len(bodyBuf))
		}
		keyLen := int(binary.LittleEndian.Uint16(bodyBuf[0:2]))
		if keyLen < MinKeyLength || keyLen > MaxKeyLength {
			return nil, 0, fmt.Errorf("%w: invalid delete key length %d", ErrInvalidRecord, keyLen)
		}
		if len(bodyBuf) != 2+keyLen {
			return nil, 0, fmt.Errorf("%w: delete body size mismatch: expected %d, got %d", ErrInvalidRecord, 2+keyLen, len(bodyBuf))
		}

		r.Key = make([]byte, keyLen)
		copy(r.Key, bodyBuf[2:2+keyLen])
	}

	return r, totalLen, nil
}
