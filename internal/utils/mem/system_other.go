//go:build !linux

package memutils

func effectiveMemory(host memorySnapshot) memorySnapshot {
	return host
}
