//go:build unix

package delivery

import (
	"os"

	"golang.org/x/sys/unix"
)

func openBatch(path string) (*os.File, error) {
	// A replaced symlink must never expose data outside the spool. Nonblocking
	// open also prevents a replaced FIFO from stalling the delivery worker.
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
