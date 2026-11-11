package stats

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/iwind/TeaGo/rands"
	"github.com/iwind/TeaGo/types"
)

func TestTrafficStatManager_Add(t *testing.T) {
	manager := NewTrafficStatManager()
	for i := 0; i < 100; i++ {
		manager.Add(1, 1, "goedge.cn", 1, 0, 0, 0, 0, 0, 0, false, 0)
	}
	t.Log(manager.itemMap)
}

func TestTrafficStatManager_Upload(t *testing.T) {
	manager := NewTrafficStatManager()
	for i := 0; i < 100; i++ {
		manager.Add(1, 1, "goedge.cn"+types.String(rands.Int(0, 10)), 1, 0, 1, 0, 0, 0, 0, false, 0)
	}
	err := manager.Upload()
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ok")
}

func BenchmarkTrafficStatManager_Add(b *testing.B) {
	runtime.GOMAXPROCS(1)

	var manager = NewTrafficStatManager()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			manager.Add(1, 1, "goedge.cn"+types.String(rand.Int63()%10), 1024, 1, 0, 0, 0, 0, 0, false, 0)
		}
	})
}

// Keep domain input outside the timed loop: allocation measurements must
// describe statistics updates, not benchmark-only random host generation.
func BenchmarkTrafficStatManagerFixedDomain(b *testing.B) {
	for _, domain := range []string{"cdn.example.test", "a-very-long-but-valid-tenant-hostname-for-the-fixed-domain-benchmark.example.test"} {
		b.Run(domain, func(b *testing.B) {
			manager := NewTrafficStatManager()
			manager.Add(42, 12345, domain, 1024, 512, 1, 0, 0, 0, 0, false, 0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				manager.Add(42, 12345, domain, 1024, 512, 1, 0, 0, 0, 0, false, 0)
			}
		})
	}
}

func BenchmarkTrafficStatManagerParallel(b *testing.B) {
	manager := NewTrafficStatManager()
	const domain = "cdn.example.test"
	manager.Add(42, 12345, domain, 1024, 512, 1, 0, 0, 0, 0, false, 0)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			manager.Add(42, 12345, domain, 1024, 512, 1, 0, 0, 0, 0, false, 0)
		}
	})
}
