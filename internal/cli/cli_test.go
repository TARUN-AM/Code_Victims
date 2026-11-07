package cli_test

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TARUN-AM/torq/internal/cli"
)

func TestCLI_AllEightCommands(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "cli_test.db")

	var stdout, stderr bytes.Buffer

	// 1. open on empty creates file and reports 0 keys
	code := cli.Run([]string{"open", dbPath}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("open failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "keys: 0") {
		t.Fatalf("expected keys: 0, got %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()

	// 2. put key val
	code = cli.Run([]string{"put", dbPath, "user:1", "alice"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("put failed: %s", stderr.String())
	}
	stdout.Reset()

	// 3. get key
	code = cli.Run([]string{"get", dbPath, "user:1"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("get failed: %s", stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "alice" {
		t.Fatalf("expected 'alice', got %q", stdout.String())
	}
	stdout.Reset()

	// 4. del key
	code = cli.Run([]string{"del", dbPath, "user:1"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("del failed: %s", stderr.String())
	}
	stdout.Reset()

	// Verify get returns NOTFOUND
	code = cli.Run([]string{"get", dbPath, "user:1"}, nil, &stdout, &stderr)
	if code != 0 || strings.TrimSpace(stdout.String()) != "NOTFOUND" {
		t.Fatalf("expected NOTFOUND, got %q", stdout.String())
	}
	stdout.Reset()

	// 5. batch file
	batchFilePath := filepath.Join(tempDir, "batch.txt")
	batchContent := "put fruit:1 apple\nput fruit:2 banana\ndel fruit:1\nput fruit:3 cherry\n"
	if err := os.WriteFile(batchFilePath, []byte(batchContent), 0666); err != nil {
		t.Fatalf("failed to write batch file: %v", err)
	}

	code = cli.Run([]string{"batch", dbPath, batchFilePath}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("batch command failed: %s", stderr.String())
	}
	stdout.Reset()

	// 6. dump (standard and --hex)
	code = cli.Run([]string{"dump", dbPath}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dump failed: %s", stderr.String())
	}
	dumpOut := stdout.String()
	if !strings.Contains(dumpOut, "fruit:2 banana") || !strings.Contains(dumpOut, "fruit:3 cherry") || strings.Contains(dumpOut, "fruit:1") {
		t.Fatalf("unexpected dump output: %s", dumpOut)
	}
	stdout.Reset()

	// dump --hex
	code = cli.Run([]string{"dump", dbPath, "--hex"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dump --hex failed: %s", stderr.String())
	}
	hexDumpOut := stdout.String()
	expectedHexKey := hex.EncodeToString([]byte("fruit:2"))
	if !strings.Contains(hexDumpOut, expectedHexKey) {
		t.Fatalf("expected hex key %s in dump: %s", expectedHexKey, hexDumpOut)
	}
	stdout.Reset()

	// 7. check
	code = cli.Run([]string{"check", dbPath}, nil, &stdout, &stderr)
	if code != 0 || strings.TrimSpace(stdout.String()) != "OK" {
		t.Fatalf("check expected OK, got %q", stdout.String())
	}
	stdout.Reset()

	// 8. compact
	code = cli.Run([]string{"compact", dbPath}, nil, &stdout, &stderr)
	if code != 0 || strings.TrimSpace(stdout.String()) != "OK" {
		t.Fatalf("compact expected OK, got %q (err: %s)", stdout.String(), stderr.String())
	}
	stdout.Reset()

	// 9. serve over an interactive pipe
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	serverDone := make(chan int, 1)
	go func() {
		serverDone <- cli.Run([]string{"serve", dbPath}, inReader, outWriter, &stderr)
		outWriter.Close()
	}()

	clientScanner := bufio.NewScanner(outReader)
	keyHex := hex.EncodeToString([]byte("serve:user"))
	valHex := hex.EncodeToString([]byte("serve:val"))

	// Step A: Send PUT op1 and wait for ACK op1
	fmt.Fprintf(inWriter, "PUT op1 %s %s\n", keyHex, valHex)
	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op1: %v", clientScanner.Err())
	}
	resp1 := clientScanner.Text()
	if !strings.HasPrefix(resp1, "ACK op1") {
		t.Fatalf("expected ACK op1, got %q", resp1)
	}

	// Step B: Send GET op2 and verify VAL
	fmt.Fprintf(inWriter, "GET op2 %s\n", keyHex)
	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op2: %v", clientScanner.Err())
	}
	resp2 := clientScanner.Text()
	if resp2 != "VAL op2 "+valHex {
		t.Fatalf("expected 'VAL op2 %s', got %q", valHex, resp2)
	}

	// Step C: Send BATCH op3
	bKey1 := hex.EncodeToString([]byte("b:k1"))
	bVal1 := hex.EncodeToString([]byte("b:v1"))
	bKey2 := hex.EncodeToString([]byte("b:k2"))
	bVal2 := hex.EncodeToString([]byte("b:v2"))
	fmt.Fprintf(inWriter, "BATCH op3 2\nput %s %s\nput %s %s\n", bKey1, bVal1, bKey2, bVal2)

	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op3: %v", clientScanner.Err())
	}
	resp3 := clientScanner.Text()
	if !strings.HasPrefix(resp3, "ACK op3") {
		t.Fatalf("expected ACK op3, got %q", resp3)
	}

	// Step D: Send GET op4 for bKey1
	fmt.Fprintf(inWriter, "GET op4 %s\n", bKey1)
	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op4: %v", clientScanner.Err())
	}
	resp4 := clientScanner.Text()
	if resp4 != "VAL op4 "+bVal1 {
		t.Fatalf("expected 'VAL op4 %s', got %q", bVal1, resp4)
	}

	// Step E: Send DEL op5
	fmt.Fprintf(inWriter, "DEL op5 %s\n", keyHex)
	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op5: %v", clientScanner.Err())
	}
	resp5 := clientScanner.Text()
	if !strings.HasPrefix(resp5, "ACK op5") {
		t.Fatalf("expected ACK op5, got %q", resp5)
	}

	// Step F: Send GET op6 and verify NOTFOUND
	fmt.Fprintf(inWriter, "GET op6 %s\n", keyHex)
	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op6: %v", clientScanner.Err())
	}
	resp6 := clientScanner.Text()
	if resp6 != "NOTFOUND op6" {
		t.Fatalf("expected 'NOTFOUND op6', got %q", resp6)
	}

	// Step G: Send COMPACT op7 and verify ACK
	fmt.Fprintf(inWriter, "COMPACT op7\n")
	if !clientScanner.Scan() {
		t.Fatalf("failed to read response for op7: %v", clientScanner.Err())
	}
	resp7 := clientScanner.Text()
	if !strings.HasPrefix(resp7, "ACK op7") {
		t.Fatalf("expected 'ACK op7 ...', got %q", resp7)
	}

	// Close stdin pipe to shut down serve
	inWriter.Close()
	select {
	case code := <-serverDone:
		if code != 0 {
			t.Fatalf("serve exited with non-zero code %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("serve did not shut down cleanly after stdin closed")
	}
}
