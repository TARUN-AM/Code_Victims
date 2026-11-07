package db

import (
	"github.com/TARUN-AM/torq/internal/failpoint"
)

// ActiveFailpoint returns the currently requested failpoint from the environment, if any.
func ActiveFailpoint() string {
	return failpoint.Active()
}

// TriggerFailpoint checks if the specified failpoint name matches KVDB_FAILPOINT.
// If active, it terminates the process with os.Exit(137) to simulate a hard kill -9.
func TriggerFailpoint(name string) {
	failpoint.Trigger(name)
}
