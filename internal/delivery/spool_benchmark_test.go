package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkSpoolPendingBacklog(b *testing.B) {
	for _, count := range []int{0, 256, 4096} {
		b.Run(fmt.Sprintf("Files%d", count), func(b *testing.B) {
			dir := b.TempDir()
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%032x.batch", i)), []byte("payload"), 0600); err != nil {
					b.Fatal(err)
				}
			}
			spool := NewSpool(dir, 1<<30, 16<<20)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := spool.Pending(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSpoolAppendBacklog(b *testing.B) {
	for _, count := range []int{0, 256, 4095} {
		b.Run(fmt.Sprintf("Files%d", count), func(b *testing.B) {
			dir := b.TempDir()
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%032x.batch", i)), []byte("payload"), 0600); err != nil {
					b.Fatal(err)
				}
			}
			spool := NewSpool(dir, 1<<30, 16<<20)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id, err := spool.Append([]byte("new payload"))
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := os.Remove(filepath.Join(dir, id+".batch")); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

// The first remote send fails: unvisited payloads must remain on disk instead
// of being eagerly allocated on each retry. File and directory sync stay enabled.
func BenchmarkSpoolDeliveryFailure(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20, 16 << 20} {
		b.Run(fmt.Sprintf("Payload%dMiB", size>>20), func(b *testing.B) {
			dir := b.TempDir()
			payload := make([]byte, size)
			for i := 0; i < 8; i++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%032x.batch", i)), payload, 0600); err != nil {
					b.Fatal(err)
				}
			}
			spool := NewSpool(dir, 256<<20, int64(size))
			failedSend := errors.New("API unavailable")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := spool.WalkPending(func(Batch) error { return failedSend })
				if !errors.Is(err, failedSend) {
					b.Fatal(err)
				}
			}
		})
	}
}
