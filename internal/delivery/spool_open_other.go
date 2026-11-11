//go:build !unix

package delivery

import "os"

func openBatch(path string) (*os.File, error) {
	// The caller checks regular-file type and matching identity before reading.
	return os.Open(path)
}
