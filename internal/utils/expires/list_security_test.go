package expires_test

import (
	"sync"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/utils/expires"
)

func TestSecurityExpiryRefreshSurvivesConcurrentGC(t *testing.T) {
	const count = 2000
	list := expires.NewSingletonList()
	for id := uint64(1); id <= count; id++ {
		list.Add(id, 100)
	}
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		defer done.Done()
		for id := uint64(1); id <= count; id++ {
			list.Add(id, 200)
		}
	}()
	go func() {
		defer done.Done()
		for i := 0; i < count; i++ {
			list.GC(100)
		}
	}()
	done.Wait()
	if list.Count() != count {
		t.Fatalf("GC removed refreshed entries: %d", list.Count())
	}
	for id := uint64(1); id <= count; id++ {
		if list.ExpiresAt(id) != 200 {
			t.Fatalf("entry %d was not refreshed", id)
		}
	}
}
