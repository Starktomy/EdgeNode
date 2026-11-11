package httpcache

import (
	"sync"
	"testing"
)

func TestFillsBoundsProducersWaitersAndReleasesCanceledFollowers(t *testing.T) {
	fills := NewFills(1, 2)
	leader, _, release, ok := fills.Join("tenant-a/server-1/key")
	if !leader || !ok {
		t.Fatal("missing producer")
	}
	if _, _, _, ok := fills.Join("other"); ok {
		t.Fatal("producer registry exceeded its budget")
	}
	_, done, leave, _ := fills.Join("tenant-a/server-1/key")
	_, _, leave2, _ := fills.Join("tenant-a/server-1/key")
	if _, _, _, ok := fills.Join("tenant-a/server-1/key"); ok {
		t.Fatal("waiter budget exceeded")
	}
	leave()
	leave()
	_, _, leave3, ok := fills.Join("tenant-a/server-1/key")
	if !ok {
		t.Fatal("canceled waiter held a slot")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	<-done
	leave2()
	leave3()
	_, _, finalRelease, ok := fills.Join("other")
	if !ok {
		t.Fatal("finished producer was retained")
	}
	finalRelease()
}
