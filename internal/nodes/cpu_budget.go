package nodes

import (
	"os"
	"runtime"
	"strconv"
)

func configureMaxCPU(maxCPU int32) int {
	return applyMaxCPU(maxCPU, runtime.NumCPU(), os.Getenv("GOMAXPROCS"), runtime.GOMAXPROCS, runtime.SetDefaultGOMAXPROCS)
}

// Runtime hooks keep tests from changing the test process's global scheduler.
func applyMaxCPU(maxCPU int32, numCPU int, env string, setProcs func(int) int, useDefault func()) int {
	if maxCPU > 0 {
		threads := min(int(maxCPU), numCPU)
		setProcs(threads)
	} else if threads, err := strconv.ParseInt(env, 10, 32); err == nil && threads > 0 {
		// SetDefaultGOMAXPROCS deliberately ignores the environment. Restore an
		// operator's positive GOMAXPROCS setting when MaxCPU returns to zero.
		setProcs(int(threads))
	} else {
		// Calling GOMAXPROCS(NumCPU()) disables container/affinity updates.
		// Restore Go's automatic quota-aware policy after an explicit cap.
		useDefault()
	}
	return setProcs(0)
}
