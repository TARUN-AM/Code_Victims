package wal

import (
	"fmt"
	"io"
	"os"
)

// Writer wraps an underlying *os.File with complete-write guarantees,
// tracking exact byte offsets and coordinating durable Syncs and truncates.
type Writer struct {
	file   *os.File
	offset int64
}

// NewWriter creates a new Writer starting at the specified file offset.
func NewWriter(file *os.File, initialOffset int64) *Writer {
	return &Writer{
		file:   file,
		offset: initialOffset,
	}
}

// Offset returns the current write offset.
func (w *Writer) Offset() int64 {
	return w.offset
}

// WriteAll ensures that all bytes in buf are written to the file,
// looping to handle partial/short writes from the OS.
func (w *Writer) WriteAll(buf []byte) error {
	totalWritten := 0
	for totalWritten < len(buf) {
		n, err := w.file.WriteAt(buf[totalWritten:], w.offset+int64(totalWritten))
		if n > 0 {
			totalWritten += n
		}
		if err != nil {
			w.offset += int64(totalWritten)
			return fmt.Errorf("short write at offset %d: %w", w.offset, err)
		}
		if n == 0 {
			w.offset += int64(totalWritten)
			return fmt.Errorf("short write at offset %d: %w", w.offset, io.ErrShortWrite)
		}
	}
	w.offset += int64(totalWritten)
	return nil
}

// WritePartial writes a specific prefix of buf (used specifically by mid_write failpoints).
func (w *Writer) WritePartial(buf []byte, count int) error {
	if count > len(buf) {
		count = len(buf)
	}
	n, err := w.file.WriteAt(buf[:count], w.offset)
	if n > 0 {
		w.offset += int64(n)
	}
	return err
}

// Sync flushes dirty file pages from the OS page cache to persistent disk.
func (w *Writer) Sync() error {
	return w.file.Sync()
}

// Truncate resizes the file to the specified byte offset and flushes the directory metadata.
func (w *Writer) Truncate(offset int64) error {
	if err := w.file.Truncate(offset); err != nil {
		return err
	}
	// Sync the file to make the truncation durable
	if err := w.file.Sync(); err != nil {
		return err
	}
	// Seek file pointer to the new truncated offset
	if _, err := w.file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	w.offset = offset
	return nil
}
