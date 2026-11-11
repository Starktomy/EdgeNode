package counters_test

import (
	"sync"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/utils/counters"
)

func TestSecurityCounterConcurrentIncrementsAreExact(t *testing.T) {
	const workers = 64
	const increments = 256
	const total = workers * increments
	counter := counters.NewCounter[uint32]()
	start := make(chan struct{})
	results := make(chan uint32, total)
	var workersDone sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		workersDone.Add(1)
		go func() {
			defer workersDone.Done()
			<-start
			for i := 0; i < increments; i++ {
				results <- counter.IncreaseKey("same-client", 86400)
			}
		}()
	}
	close(start)
	workersDone.Wait()
	close(results)
	seen := make([]bool, total+1)
	for result := range results {
		if result == 0 || result > total || seen[result] {
			t.Fatalf("non-unique increment result %d", result)
		}
		seen[result] = true
	}
	if got := counter.GetKey("same-client"); got != total {
		t.Fatalf("count = %d, want %d", got, total)
	}
}
