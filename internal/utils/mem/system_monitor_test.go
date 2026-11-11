package memutils

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMemoryMonitorConcurrentInitialization(t *testing.T) {
	var reads atomic.Int64
	var applied atomic.Uint64
	monitor := memoryMonitor{
		readMemory: func() (memorySnapshot, error) {
			reads.Add(1)
			return memorySnapshot{totalBytes: 512 << 20, availableBytes: 128 << 20}, nil
		},
		applyLimit: applied.Store,
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			if got := monitor.snapshot(); got.totalBytes != 512<<20 || got.availableBytes != 128<<20 {
				t.Errorf("unexpected snapshot: %+v", got)
			}
		})
	}
	workers.Wait()
	if reads.Load() != 1 || applied.Load() != 512<<20 {
		t.Fatalf("initial reads=%d, applied=%d", reads.Load(), applied.Load())
	}
}

func TestMemoryMonitorRefresh(t *testing.T) {
	var next memorySnapshot
	var nextErr error
	var limits []uint64
	monitor := memoryMonitor{
		readMemory: func() (memorySnapshot, error) { return next, nextErr },
		applyLimit: func(total uint64) { limits = append(limits, total) },
	}
	if got := monitor.snapshot(); got.totalBytes != 0 || len(limits) != 0 {
		t.Fatalf("unknown initial memory must not apply a limit: %+v, %v", got, limits)
	}
	next = memorySnapshot{totalBytes: 512 << 20, availableBytes: 768 << 20}
	monitor.refresh()
	if got := monitor.snapshot(); got.totalBytes != 512<<20 || got.availableBytes != 512<<20 {
		t.Fatalf("refresh did not clamp available bytes: %+v", got)
	}
	next = memorySnapshot{totalBytes: 256 << 20, availableBytes: 32 << 20}
	monitor.refresh()
	previous := monitor.snapshot()
	if previous.totalBytes != 256<<20 || previous.availableBytes != 32<<20 {
		t.Fatalf("dynamic budget was not refreshed: %+v", previous)
	}
	nextErr = errors.New("host statistics unavailable")
	monitor.refresh()
	if monitor.snapshot() != previous || len(limits) != 2 {
		t.Fatalf("transient read failure discarded valid budget or reset limit: %v", limits)
	}
	nextErr = nil
	next = memorySnapshot{}
	monitor.refresh()
	if monitor.snapshot() != previous || len(limits) != 2 {
		t.Fatal("zero total discarded valid budget or installed a zero limit")
	}
}

func TestMemoryMonitorConcurrentRefresh(t *testing.T) {
	var reads uint64 // refresh serializes accesses to the reader
	monitor := memoryMonitor{
		readMemory: func() (memorySnapshot, error) {
			reads++
			return memorySnapshot{totalBytes: reads << 20, availableBytes: reads << 19}, nil
		},
	}
	_ = monitor.snapshot()
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 100 {
				got := monitor.snapshot()
				if got.totalBytes != 2*got.availableBytes {
					t.Error("reader saw a partially published snapshot")
				}
			}
		})
	}
	for range 2 {
		workers.Go(func() {
			for range 100 {
				monitor.refresh()
			}
		})
	}
	workers.Wait()
}

func TestMemoryGBCompatibility(t *testing.T) {
	for _, test := range []struct {
		bytes uint64
		want  int
	}{{0, 1}, {512 << 20, 1}, {1 << 30, 1}, {3<<30 + 512<<20, 3}} {
		if got := memoryGB(test.bytes); got != test.want {
			t.Errorf("memoryGB(%d)=%d, want %d", test.bytes, got, test.want)
		}
	}
}

var cachedMemoryBenchmark uint64

func BenchmarkSystemMemoryBytesCached(b *testing.B) {
	_ = SystemMemoryBytes()
	b.ReportAllocs()
	b.ResetTimer()
	var total uint64
	for b.Loop() {
		total = SystemMemoryBytes()
	}
	cachedMemoryBenchmark = total
}

func BenchmarkSystemMemoryBytesParallel(b *testing.B) {
	_ = SystemMemoryBytes()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = SystemMemoryBytes()
		}
	})
}
