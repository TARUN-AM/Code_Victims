package cli

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TARUN-AM/torq/internal/db"
	"github.com/TARUN-AM/torq/internal/limits"
	"github.com/TARUN-AM/torq/internal/recovery"
)

// Run parses command line arguments and executes the requested kvdb command.
// Diagnostics go to stderr; results and serve protocol responses go to stdout.
func Run(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 1
	}

	command := args[0]
	cmdArgs := args[1:]

	switch command {
	case "open":
		return runOpen(cmdArgs, stdout, stderr)
	case "put":
		return runPut(cmdArgs, stdout, stderr)
	case "get":
		return runGet(cmdArgs, stdout, stderr)
	case "del":
		return runDel(cmdArgs, stdout, stderr)
	case "batch":
		return runBatch(cmdArgs, stdout, stderr)
	case "dump":
		return runDump(cmdArgs, stdout, stderr)
	case "check":
		return runCheck(cmdArgs, stdout, stderr)
	case "compact":
		return runCompact(cmdArgs, stdout, stderr)
	case "serve":
		return runServe(cmdArgs, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", command)
		printUsage(stderr)
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage:
  kvdb open <path> [--max-bytes N]
  kvdb put <path> <key> <value> (or --db <path> --key <key> --value <value>)
  kvdb get <path> <key>         (or --db <path> --key <key>)
  kvdb del <path> <key>         (or --db <path> --key <key>)
  kvdb batch <path> <file>
  kvdb dump <path> [--hex]
  kvdb check <path>
  kvdb compact <path>           (or --db <path>)
  kvdb serve <path> [--max-bytes N]
`)
}

type kvArgs struct {
	path   string
	key    string
	val    string
	hasVal bool
}

func parseKVArgs(args []string) kvArgs {
	var res kvArgs
	var positionals []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--db" || arg == "-db" {
			if i+1 < len(args) {
				res.path = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--db=") {
			res.path = strings.TrimPrefix(arg, "--db=")
		} else if arg == "--key" || arg == "-key" || arg == "-k" || arg == "--k" {
			if i+1 < len(args) {
				res.key = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--key=") {
			res.key = strings.TrimPrefix(arg, "--key=")
		} else if arg == "--value" || arg == "-value" || arg == "-val" || arg == "--val" || arg == "-v" || arg == "--v" {
			if i+1 < len(args) {
				res.val = args[i+1]
				res.hasVal = true
				i++
			}
		} else if strings.HasPrefix(arg, "--value=") {
			res.val = strings.TrimPrefix(arg, "--value=")
			res.hasVal = true
		} else {
			positionals = append(positionals, arg)
		}
	}

	posIdx := 0
	if res.path == "" && posIdx < len(positionals) {
		res.path = positionals[posIdx]
		posIdx++
	}
	if res.key == "" && posIdx < len(positionals) {
		res.key = positionals[posIdx]
		posIdx++
	}
	if !res.hasVal && posIdx < len(positionals) {
		res.val = positionals[posIdx]
		res.hasVal = true
		posIdx++
	}

	return res
}

func parseMaxBytes(args []string) (int64, []string, error) {
	var maxBytes int64 = 0
	cleanArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--max-bytes" {
			if i+1 >= len(args) {
				return 0, nil, errors.New("--max-bytes requires an integer value")
			}
			val, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("invalid --max-bytes value: %w", err)
			}
			maxBytes = val
			i++
		} else if strings.HasPrefix(args[i], "--max-bytes=") {
			valStr := strings.TrimPrefix(args[i], "--max-bytes=")
			val, err := strconv.ParseInt(valStr, 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("invalid --max-bytes value: %w", err)
			}
			maxBytes = val
		} else {
			cleanArgs = append(cleanArgs, args[i])
		}
	}
	return maxBytes, cleanArgs, nil
}

// kvdb open <path> [--max-bytes N]
func runOpen(args []string, stdout io.Writer, stderr io.Writer) int {
	maxBytes, cleanArgs, err := parseMaxBytes(args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(cleanArgs) < 1 {
		fmt.Fprintf(stderr, "usage: kvdb open <path> [--max-bytes N]\n")
		return 1
	}
	path := cleanArgs[0]

	start := time.Now()
	database, err := db.Open(path, db.Options{MaxBytes: maxBytes})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	elapsed := time.Since(start)
	keyCount := database.KeyCount()
	fmt.Fprintf(stdout, "keys: %d, recovery_time: %v\n", keyCount, elapsed)
	return 0
}

// kvdb put <path> <key> <value> (or --db path --key k --value v)
func runPut(args []string, stdout io.Writer, stderr io.Writer) int {
	kv := parseKVArgs(args)
	if kv.path == "" || kv.key == "" || !kv.hasVal {
		fmt.Fprintf(stderr, "usage: kvdb put <path> <key> <value> (or --db <path> --key <key> --value <value>)\n")
		return 1
	}

	database, err := db.Open(kv.path, db.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	if err := database.Put([]byte(kv.key), []byte(kv.val)); err != nil {
		fmt.Fprintf(stderr, "put failed: %v\n", err)
		return 1
	}
	return 0
}

// kvdb get <path> <key> (or --db path --key k)
func runGet(args []string, stdout io.Writer, stderr io.Writer) int {
	kv := parseKVArgs(args)
	if kv.path == "" || kv.key == "" {
		fmt.Fprintf(stderr, "usage: kvdb get <path> <key> (or --db <path> --key <key>)\n")
		return 1
	}

	database, err := db.Open(kv.path, db.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	val, err := database.Get([]byte(kv.key))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			fmt.Fprintln(stdout, "NOTFOUND")
			return 0
		}
		fmt.Fprintf(stderr, "get failed: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, string(val))
	return 0
}

// kvdb del <path> <key> (or --db path --key k)
func runDel(args []string, stdout io.Writer, stderr io.Writer) int {
	kv := parseKVArgs(args)
	if kv.path == "" || kv.key == "" {
		fmt.Fprintf(stderr, "usage: kvdb del <path> <key> (or --db <path> --key <key>)\n")
		return 1
	}

	database, err := db.Open(kv.path, db.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	if err := database.Delete([]byte(kv.key)); err != nil {
		fmt.Fprintf(stderr, "del failed: %v\n", err)
		return 1
	}
	return 0
}

// kvdb batch <path> <file>
func runBatch(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintf(stderr, "usage: kvdb batch <path> <file>\n")
		return 1
	}
	path := args[0]
	batchFilePath := args[1]

	f, err := os.Open(batchFilePath)
	if err != nil {
		fmt.Fprintf(stderr, "failed to open batch file %q: %v\n", batchFilePath, err)
		return 1
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var ops []db.BatchOp

	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if len(line) > 0 {
			parts := strings.SplitN(line, " ", 3)
			cmd := parts[0]
			switch strings.ToLower(cmd) {
			case "put":
				if len(parts) < 3 {
					fmt.Fprintf(stderr, "malformed put line in batch: %s\n", line)
					return 1
				}
				ops = append(ops, db.BatchOp{
					Type:  db.OpPut,
					Key:   []byte(parts[1]),
					Value: []byte(parts[2]),
				})
			case "del":
				if len(parts) < 2 {
					fmt.Fprintf(stderr, "malformed del line in batch: %s\n", line)
					return 1
				}
				ops = append(ops, db.BatchOp{
					Type: db.OpDelete,
					Key:  []byte(parts[1]),
				})
			default:
				fmt.Fprintf(stderr, "unknown op in batch file: %s\n", cmd)
				return 1
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			fmt.Fprintf(stderr, "error reading batch file: %v\n", err)
			return 1
		}
	}

	database, err := db.Open(path, db.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	if err := database.Batch(ops); err != nil {
		fmt.Fprintf(stderr, "batch failed: %v\n", err)
		return 1
	}
	return 0
}

// kvdb dump <path> [--hex]
func runDump(args []string, stdout io.Writer, stderr io.Writer) int {
	isHex := false
	cleanArgs := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--hex" {
			isHex = true
		} else {
			cleanArgs = append(cleanArgs, a)
		}
	}
	if len(cleanArgs) < 1 {
		fmt.Fprintf(stderr, "usage: kvdb dump <path> [--hex]\n")
		return 1
	}
	path := cleanArgs[0]

	database, err := db.Open(path, db.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	keys := database.AllKeys()
	sort.Strings(keys)

	for _, k := range keys {
		val, err := database.Get([]byte(k))
		if err != nil {
			continue
		}
		if isHex {
			fmt.Fprintf(stdout, "%s %s\n", hex.EncodeToString([]byte(k)), hex.EncodeToString(val))
		} else {
			fmt.Fprintf(stdout, "%s %s\n", k, string(val))
		}
	}
	return 0
}

// kvdb check <path>
func runCheck(args []string, stdout io.Writer, stderr io.Writer) int {
	kv := parseKVArgs(args)
	if kv.path == "" {
		fmt.Fprintf(stderr, "usage: kvdb check <path>\n")
		return 1
	}

	res, err := recovery.Check(kv.path)
	if err != nil {
		fmt.Fprintf(stderr, "check error: %v\n", err)
		return 1
	}

	if res.Valid {
		fmt.Fprintln(stdout, "OK")
		return 0
	}

	fmt.Fprintf(stdout, "CORRUPT: %s (committed_bytes: %d, file_size: %d)\n", res.Message, res.CommittedBytes, res.FileSize)
	return 1
}

// kvdb compact <path> (or --db path)
func runCompact(args []string, stdout io.Writer, stderr io.Writer) int {
	kv := parseKVArgs(args)
	if kv.path == "" {
		fmt.Fprintf(stderr, "usage: kvdb compact <path> (or --db <path>)\n")
		return 1
	}

	database, err := db.Open(kv.path, db.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	if err := database.Compact(); err != nil {
		fmt.Fprintf(stderr, "compact failed: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "OK")
	return 0
}

// kvdb serve <path> [--max-bytes N]
// Line-based server protocol using bufio.Reader (handles lines up to multi-megabytes)
func runServe(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	maxBytes, cleanArgs, err := parseMaxBytes(args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(cleanArgs) < 1 {
		fmt.Fprintf(stderr, "usage: kvdb serve <path> [--max-bytes N]\n")
		return 1
	}
	path := cleanArgs[0]

	database, err := db.Open(path, db.Options{MaxBytes: maxBytes})
	if err != nil {
		fmt.Fprintf(stderr, "open failed: %v\n", err)
		return 1
	}
	defer database.Close()

	// 4 MB buffer for large 1 MB values (2 MB hex characters)
	reader := bufio.NewReaderSize(stdin, 4*1024*1024)
	var outMu sync.Mutex
	var wg sync.WaitGroup

	respond := func(msg string) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintln(stdout, msg)
	}

	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if len(line) > 0 {
			parts := strings.SplitN(line, " ", 4)
			cmd := strings.ToUpper(parts[0])

			switch cmd {
			case "PUT":
				if len(parts) < 3 {
					respond("ERR ? PROTOCOL invalid PUT line: expected PUT <id> <hexkey> [hexvalue]")
					continue
				}
				id := parts[1]
				hexKey := parts[2]
				hexVal := ""
				if len(parts) >= 4 {
					hexVal = parts[3]
				}

				key, err1 := hex.DecodeString(hexKey)
				val, err2 := hex.DecodeString(hexVal)
				if err1 != nil || err2 != nil {
					respond(fmt.Sprintf("ERR %s BADHEX invalid hex encoding in key/value", id))
					continue
				}

				// Execute Put
				wg.Add(1)
				go func(opID string, k, v []byte) {
					defer wg.Done()
					err := database.Put(k, v)
					if err != nil {
						if errors.Is(err, limits.ErrDiskFull) {
							respond(fmt.Sprintf("ERR %s DISKFULL %v", opID, err))
						} else {
							respond(fmt.Sprintf("ERR %s WRITE_ERROR %v", opID, err))
						}
						return
					}
					// Acknowledged strictly after commit sync
					txnID := database.LastTxnID()
					respond(fmt.Sprintf("ACK %s %d", opID, txnID))
				}(id, key, val)

			case "DEL":
				if len(parts) < 3 {
					respond("ERR ? PROTOCOL invalid DEL line: expected DEL <id> <hexkey>")
					continue
				}
				id := parts[1]
				hexKey := parts[2]
				key, err := hex.DecodeString(hexKey)
				if err != nil {
					respond(fmt.Sprintf("ERR %s BADHEX invalid hex key", id))
					continue
				}

				wg.Add(1)
				go func(opID string, k []byte) {
					defer wg.Done()
					err := database.Delete(k)
					if err != nil {
						if errors.Is(err, limits.ErrDiskFull) {
							respond(fmt.Sprintf("ERR %s DISKFULL %v", opID, err))
						} else {
							respond(fmt.Sprintf("ERR %s WRITE_ERROR %v", opID, err))
						}
						return
					}
					txnID := database.LastTxnID()
					respond(fmt.Sprintf("ACK %s %d", opID, txnID))
				}(id, key)

			case "BATCH":
				if len(parts) < 3 {
					respond("ERR ? PROTOCOL invalid BATCH line: expected BATCH <id> <n>")
					continue
				}
				id := parts[1]
				n, err := strconv.Atoi(parts[2])
				if err != nil || n < 1 {
					respond(fmt.Sprintf("ERR %s BADCOUNT invalid batch count", id))
					continue
				}

				// Read n batch lines
				batchOps := make([]db.BatchOp, n)
				batchReadErr := false

				for i := 0; i < n; i++ {
					bLine, bErr := reader.ReadString('\n')
					if bErr != nil && !errors.Is(bErr, io.EOF) {
						batchReadErr = true
						break
					}
					bLine = strings.TrimRight(bLine, "\r\n")
					bParts := strings.SplitN(bLine, " ", 3)
					if len(bParts) < 2 {
						batchReadErr = true
						break
					}
					bType := strings.ToLower(bParts[0])
					k, errK := hex.DecodeString(bParts[1])
					if errK != nil {
						batchReadErr = true
						break
					}
					if bType == "put" {
						if len(bParts) < 2 {
							batchReadErr = true
							break
						}
						hexVal := ""
						if len(bParts) >= 3 {
							hexVal = bParts[2]
						}
						v, errV := hex.DecodeString(hexVal)
						if errV != nil {
							batchReadErr = true
							break
						}
						batchOps[i] = db.BatchOp{Type: db.OpPut, Key: k, Value: v}
					} else if bType == "del" {
						batchOps[i] = db.BatchOp{Type: db.OpDelete, Key: k}
					} else {
						batchReadErr = true
						break
					}
				}

				if batchReadErr {
					respond(fmt.Sprintf("ERR %s BATCH_FORMAT error reading batch sub-lines", id))
					continue
				}

				wg.Add(1)
				go func(opID string, ops []db.BatchOp) {
					defer wg.Done()
					err := database.Batch(ops)
					if err != nil {
						if errors.Is(err, limits.ErrDiskFull) {
							respond(fmt.Sprintf("ERR %s DISKFULL %v", opID, err))
						} else {
							respond(fmt.Sprintf("ERR %s BATCH_ERROR %v", opID, err))
						}
						return
					}
					txnID := database.LastTxnID()
					respond(fmt.Sprintf("ACK %s %d", opID, txnID))
				}(id, batchOps)

			case "GET":
				if len(parts) < 3 {
					respond("ERR ? PROTOCOL invalid GET line: expected GET <id> <hexkey>")
					continue
				}
				id := parts[1]
				hexKey := parts[2]
				key, err := hex.DecodeString(hexKey)
				if err != nil {
					respond(fmt.Sprintf("ERR %s BADHEX invalid hex key", id))
					continue
				}

				wg.Add(1)
				go func(opID string, k []byte) {
					defer wg.Done()
					val, err := database.Get(k)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							respond(fmt.Sprintf("NOTFOUND %s", opID))
						} else {
							respond(fmt.Sprintf("ERR %s GET_ERROR %v", opID, err))
						}
						return
					}
					respond(fmt.Sprintf("VAL %s %s", opID, hex.EncodeToString(val)))
				}(id, key)

			case "COMPACT":
				if len(parts) < 2 {
					respond("ERR ? PROTOCOL invalid COMPACT line: expected COMPACT <id>")
					continue
				}
				id := parts[1]
				wg.Add(1)
				go func(opID string) {
					defer wg.Done()
					if err := database.Compact(); err != nil {
						respond(fmt.Sprintf("ERR %s COMPACT_ERROR %v", opID, err))
						return
					}
					respond(fmt.Sprintf("ACK %s %d", opID, database.LastTxnID()))
				}(id)

			default:
				respond(fmt.Sprintf("ERR ? UNKNOWN_CMD %s", cmd))
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			fmt.Fprintf(stderr, "serve read error: %v\n", err)
			break
		}
	}

	wg.Wait()
	return 0
}
