package wal

import (
	"bytes"
	"errors"
	"testing"
)

func TestHeader_RoundTrip(t *testing.T) {
	buf := make([]byte, FileHeaderSize)
	if err := EncodeFileHeader(buf); err != nil {
		t.Fatalf("EncodeFileHeader failed: %v", err)
	}

	h, err := DecodeFileHeader(buf)
	if err != nil {
		t.Fatalf("DecodeFileHeader failed: %v", err)
	}

	if string(h.Magic[:]) != FileHeaderMagic {
		t.Errorf("expected magic %s, got %s", FileHeaderMagic, string(h.Magic[:]))
	}
	if h.Version != FormatVersion {
		t.Errorf("expected version %d, got %d", FormatVersion, h.Version)
	}
	if h.HeaderSize != FileHeaderSize {
		t.Errorf("expected header size %d, got %d", FileHeaderSize, h.HeaderSize)
	}

	// Test corruption detection
	corruptBuf := make([]byte, FileHeaderSize)
	copy(corruptBuf, buf)
	corruptBuf[0] = 'X' // corrupt magic
	if _, err := DecodeFileHeader(corruptBuf); !errors.Is(err, ErrCorruptHeader) {
		t.Errorf("expected ErrCorruptHeader on bad magic, got %v", err)
	}

	copy(corruptBuf, buf)
	corruptBuf[4] = 99 // unsupported version
	if _, err := DecodeFileHeader(corruptBuf); !errors.Is(err, ErrCorruptHeader) {
		t.Errorf("expected ErrCorruptHeader on bad version, got %v", err)
	}

	copy(corruptBuf, buf)
	corruptBuf[15] ^= 0x01 // bit flip in CRC
	if _, err := DecodeFileHeader(corruptBuf); !errors.Is(err, ErrCorruptHeader) {
		t.Errorf("expected ErrCorruptHeader on bad CRC, got %v", err)
	}

	// Incomplete header
	if _, err := DecodeFileHeader(buf[:10]); !errors.Is(err, ErrIncompleteRecord) {
		t.Errorf("expected ErrIncompleteRecord for short buffer, got %v", err)
	}
}

func TestRecord_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		rec  *Record
	}{
		{
			name: "BEGIN transaction",
			rec: &Record{
				Type:    RecordTypeBegin,
				TxnID:   1001,
				OpCount: 5,
			},
		},
		{
			name: "PUT standard key and value",
			rec: &Record{
				Type:  RecordTypePut,
				TxnID: 1001,
				Key:   []byte("user:9421:profile"),
				Value: []byte(`{"name":"Alice","tier":"enterprise"}`),
			},
		},
		{
			name: "PUT empty value (0 bytes)",
			rec: &Record{
				Type:  RecordTypePut,
				TxnID: 1001,
				Key:   []byte("flag:active"),
				Value: []byte{},
			},
		},
		{
			name: "PUT maximum key size (1024 bytes)",
			rec: &Record{
				Type:  RecordTypePut,
				TxnID: 1001,
				Key:   bytes.Repeat([]byte("k"), MaxKeyLength),
				Value: []byte("val"),
			},
		},
		{
			name: "PUT maximum value size (1 MB)",
			rec: &Record{
				Type:  RecordTypePut,
				TxnID: 1001,
				Key:   []byte("blob:large"),
				Value: bytes.Repeat([]byte("V"), MaxValueLength),
			},
		},
		{
			name: "DELETE record",
			rec: &Record{
				Type:  RecordTypeDelete,
				TxnID: 1001,
				Key:   []byte("user:9421:session"),
			},
		},
		{
			name: "DELETE record with max key size",
			rec: &Record{
				Type:  RecordTypeDelete,
				TxnID: 1001,
				Key:   bytes.Repeat([]byte("d"), MaxKeyLength),
			},
		},
		{
			name: "COMMIT transaction",
			rec: &Record{
				Type:    RecordTypeCommit,
				TxnID:   1001,
				OpCount: 5,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeRecord(tc.rec)
			if err != nil {
				t.Fatalf("EncodeRecord failed: %v", err)
			}

			decoded, bytesRead, err := DecodeRecordFromBytes(encoded)
			if err != nil {
				t.Fatalf("DecodeRecordFromBytes failed: %v", err)
			}

			if bytesRead != len(encoded) {
				t.Fatalf("bytesRead mismatch: expected %d, got %d", len(encoded), bytesRead)
			}

			if decoded.Type != tc.rec.Type {
				t.Errorf("type mismatch: expected %d, got %d", tc.rec.Type, decoded.Type)
			}
			if decoded.TxnID != tc.rec.TxnID {
				t.Errorf("txnID mismatch: expected %d, got %d", tc.rec.TxnID, decoded.TxnID)
			}
			if decoded.OpCount != tc.rec.OpCount {
				t.Errorf("opCount mismatch: expected %d, got %d", tc.rec.OpCount, decoded.OpCount)
			}
			if !bytes.Equal(decoded.Key, tc.rec.Key) {
				t.Errorf("key mismatch")
			}
			if !bytes.Equal(decoded.Value, tc.rec.Value) {
				t.Errorf("value mismatch")
			}
		})
	}
}

func TestRecord_ValidationAndLimits(t *testing.T) {
	// 1. Empty key check
	emptyKeyRec := &Record{Type: RecordTypePut, TxnID: 1, Key: []byte{}, Value: []byte("val")}
	if _, err := EncodeRecord(emptyKeyRec); !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("expected ErrKeyEmpty, got %v", err)
	}

	// 2. Oversized key (> 1024 bytes)
	tooLongKey := make([]byte, MaxKeyLength+1)
	oversizedKeyRec := &Record{Type: RecordTypePut, TxnID: 1, Key: tooLongKey, Value: []byte("val")}
	if _, err := EncodeRecord(oversizedKeyRec); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge, got %v", err)
	}

	// 3. Oversized value (> 1 MB)
	tooLongVal := make([]byte, MaxValueLength+1)
	oversizedValRec := &Record{Type: RecordTypePut, TxnID: 1, Key: []byte("key"), Value: tooLongVal}
	if _, err := EncodeRecord(oversizedValRec); !errors.Is(err, ErrValueTooLarge) {
		t.Errorf("expected ErrValueTooLarge, got %v", err)
	}

	// 4. Unknown record type
	unknownRec := &Record{Type: 99, TxnID: 1}
	if _, err := EncodeRecord(unknownRec); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("expected ErrInvalidRecord for type 99, got %v", err)
	}
}

func TestRecord_Corruptions(t *testing.T) {
	rec := &Record{
		Type:  RecordTypePut,
		TxnID: 42,
		Key:   []byte("config:timeout"),
		Value: []byte("30s"),
	}
	encoded, err := EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// 1. Corrupt marker
	corruptMarker := make([]byte, len(encoded))
	copy(corruptMarker, encoded)
	corruptMarker[0] = 0x00
	if _, _, err := DecodeRecordFromBytes(corruptMarker); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("expected ErrInvalidRecord for bad marker, got %v", err)
	}

	// 2. Corrupt CRC32 (flip single bit in trailer)
	corruptCRC := make([]byte, len(encoded))
	copy(corruptCRC, encoded)
	corruptCRC[len(corruptCRC)-1] ^= 0x01
	if _, _, err := DecodeRecordFromBytes(corruptCRC); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("expected ErrInvalidRecord for bad CRC, got %v", err)
	}

	// 3. Corrupt body (flip single bit in payload)
	corruptBody := make([]byte, len(encoded))
	copy(corruptBody, encoded)
	corruptBody[16] ^= 0x01
	if _, _, err := DecodeRecordFromBytes(corruptBody); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("expected ErrInvalidRecord for corrupted body, got %v", err)
	}

	// 4. Corrupt record type in header
	corruptType := make([]byte, len(encoded))
	copy(corruptType, encoded)
	corruptType[1] = 0 // invalid type
	// Recompute CRC so type check triggers specifically
	crc := CalculateChecksum(corruptType[:len(corruptType)-4])
	corruptType[len(corruptType)-4] = byte(crc)
	corruptType[len(corruptType)-3] = byte(crc >> 8)
	corruptType[len(corruptType)-2] = byte(crc >> 16)
	corruptType[len(corruptType)-1] = byte(crc >> 24)
	if _, _, err := DecodeRecordFromBytes(corruptType); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("expected ErrInvalidRecord for invalid type, got %v", err)
	}
}

func TestRecord_TruncationSweep(t *testing.T) {
	// A TRUNCATION SWEEP: decode every prefix of an encoded record;
	// only the full record may decode as valid.
	testRecords := []*Record{
		{
			Type:    RecordTypeBegin,
			TxnID:   50,
			OpCount: 1,
		},
		{
			Type:  RecordTypePut,
			TxnID: 50,
			Key:   []byte("alpha"),
			Value: []byte("beta-gamma-delta"),
		},
		{
			Type:  RecordTypePut,
			TxnID: 50,
			Key:   bytes.Repeat([]byte("k"), 128),
			Value: bytes.Repeat([]byte("v"), 1024),
		},
		{
			Type:  RecordTypeDelete,
			TxnID: 50,
			Key:   []byte("alpha"),
		},
		{
			Type:    RecordTypeCommit,
			TxnID:   50,
			OpCount: 1,
		},
	}

	for _, rec := range testRecords {
		encoded, err := EncodeRecord(rec)
		if err != nil {
			t.Fatalf("failed to encode record: %v", err)
		}

		fullLen := len(encoded)

		// Sweep every prefix from 0 to fullLen-1
		for prefixLen := 0; prefixLen < fullLen; prefixLen++ {
			prefix := encoded[:prefixLen]
			_, _, err := DecodeRecordFromBytes(prefix)
			if err == nil {
				t.Fatalf("prefix of length %d / %d decoded as valid; expected ErrIncompleteRecord", prefixLen, fullLen)
			}
			if !errors.Is(err, ErrIncompleteRecord) {
				t.Fatalf("prefix of length %d / %d returned %v; expected ErrIncompleteRecord", prefixLen, fullLen, err)
			}
		}

		// Full record MUST decode successfully
		decoded, bytesRead, err := DecodeRecordFromBytes(encoded)
		if err != nil {
			t.Fatalf("full record of length %d failed to decode: %v", fullLen, err)
		}
		if bytesRead != fullLen {
			t.Fatalf("bytesRead (%d) != fullLen (%d)", bytesRead, fullLen)
		}
		if decoded.Type != rec.Type {
			t.Fatalf("decoded type %d != expected %d", decoded.Type, rec.Type)
		}
	}
}

func TestReader_StreamAndOffsets(t *testing.T) {
	var buf bytes.Buffer

	// 1. Write header
	headerBytes := make([]byte, FileHeaderSize)
	if err := EncodeFileHeader(headerBytes); err != nil {
		t.Fatalf("EncodeFileHeader failed: %v", err)
	}
	buf.Write(headerBytes)

	// 2. Write sequence: BEGIN, PUT, DELETE, COMMIT
	rBegin := &Record{Type: RecordTypeBegin, TxnID: 1, OpCount: 2}
	bBegin, _ := EncodeRecord(rBegin)
	buf.Write(bBegin)

	rPut := &Record{Type: RecordTypePut, TxnID: 1, Key: []byte("foo"), Value: []byte("bar_payload")}
	bPut, _ := EncodeRecord(rPut)
	buf.Write(bPut)

	rDel := &Record{Type: RecordTypeDelete, TxnID: 1, Key: []byte("foo")}
	bDel, _ := EncodeRecord(rDel)
	buf.Write(bDel)

	rCommit := &Record{Type: RecordTypeCommit, TxnID: 1, OpCount: 2}
	bCommit, _ := EncodeRecord(rCommit)
	buf.Write(bCommit)

	// 3. Scan with reader
	rawReader := bytes.NewReader(buf.Bytes())
	// Skip header
	headerRead := make([]byte, FileHeaderSize)
	rawReader.Read(headerRead)

	reader := NewReader(rawReader, FileHeaderSize)

	// Check BEGIN
	rec1, loc1, err := reader.Next()
	if err != nil {
		t.Fatalf("reading BEGIN failed: %v", err)
	}
	if rec1.Type != RecordTypeBegin || loc1.Offset != FileHeaderSize {
		t.Errorf("BEGIN loc/type mismatch: got offset %d, type %d", loc1.Offset, rec1.Type)
	}

	// Check PUT
	rec2, loc2, err := reader.Next()
	if err != nil {
		t.Fatalf("reading PUT failed: %v", err)
	}
	if rec2.Type != RecordTypePut {
		t.Errorf("PUT type mismatch")
	}
	// Verify ValueOffset points to "bar_payload"
	expectedVal := "bar_payload"
	valBytes := buf.Bytes()[loc2.ValueOffset : loc2.ValueOffset+int64(loc2.ValueLength)]
	if string(valBytes) != expectedVal {
		t.Errorf("loc.ValueOffset mismatch: expected %q, read %q", expectedVal, string(valBytes))
	}

	// Check DELETE
	rec3, _, err := reader.Next()
	if err != nil {
		t.Fatalf("reading DELETE failed: %v", err)
	}
	if rec3.Type != RecordTypeDelete {
		t.Errorf("DELETE type mismatch")
	}

	// Check COMMIT
	rec4, _, err := reader.Next()
	if err != nil {
		t.Fatalf("reading COMMIT failed: %v", err)
	}
	if rec4.Type != RecordTypeCommit {
		t.Errorf("COMMIT type mismatch")
	}

	// Next must be clean EOF
	_, _, err = reader.Next()
	if !errors.Is(err, errors.New("EOF")) && err.Error() != "EOF" {
		t.Errorf("expected clean EOF, got %v", err)
	}
}
