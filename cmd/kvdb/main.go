package main

import (
	"os"

	"github.com/TARUN-AM/torq/internal/cli"
)

func main() {
	exitCode := cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(exitCode)
}
