package failpoint

import "os"

// Active returns the currently requested failpoint name from KVDB_FAILPOINT.
func Active() string {
	return os.Getenv("KVDB_FAILPOINT")
}

// Trigger checks if name matches KVDB_FAILPOINT.
// If active, it terminates the process immediately with os.Exit(137).
func Trigger(name string) {
	if Active() == name {
		os.Exit(137)
	}
}
