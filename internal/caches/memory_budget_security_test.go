package caches

import (
	"sync"
	"testing"

	memutils "github.com/TeaOSLab/EdgeNode/internal/utils/mem"
)

func TestSecurityCacheMemoryBudgetFitsSmallAndSharedLimits(t *testing.T) {
	for _, tc := range []struct {
		total uint64
		count int64
		want  int64
	}{
		{512 << 20, 1, (512 << 20) / 3},
		{512 << 20, 4, (512 << 20) / 3 / 4},
		{16 << 30, 8, (16 << 30) / 3 / 8},
		{128 << 20, 0, (128 << 20) / 3},
	} {
		got := memoryStorageBudget(tc.total, tc.count)
		if got != tc.want || uint64(got) > tc.total/3 {
			t.Fatalf("memory budget=%d, want=%d for total=%d count=%d", got, tc.want, tc.total, tc.count)
		}
	}
}

func TestSecurityCacheBudgetCountIsSafeDuringPolicyRefresh(t *testing.T) {
	manager := NewManager()
	manager.storageMap[1] = &MemoryStorage{}
	manager.refreshStorageCounts()
	total := memutils.SystemMemoryBytes()
	oneStorage, twoStorages := memoryStorageBudget(total, 1), memoryStorageBudget(total, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := 0; n < 1000; n++ {
			manager.locker.Lock()
			if n%2 == 0 {
				manager.storageMap[2] = &MemoryStorage{}
			} else {
				delete(manager.storageMap, 2)
			}
			manager.refreshStorageCounts()
			manager.locker.Unlock()
		}
	})
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 1000; n++ {
				if got := manager.MaxSystemMemoryBytesPerStorage(); got != oneStorage && got != twoStorages {
					t.Errorf("invalid cache budget during policy refresh: %d", got)
				}
			}
		}()
	}
	wg.Wait()
}

func TestSecurityCacheBudgetExcludesDiskOnlyPolicies(t *testing.T) {
	manager := NewManager()
	manager.storageMap[1] = &MemoryStorage{}
	manager.storageMap[2] = &FileStorage{memoryStorage: &MemoryStorage{}}
	for id := int64(3); id < 103; id++ {
		manager.storageMap[id] = &FileStorage{}
	}
	manager.refreshStorageCounts()
	if manager.CountFileStorages != 101 || manager.CountMemoryStorages != 2 || manager.memoryStorageCount.Load() != 2 {
		t.Fatalf("disk-only policies consumed a payload memory share: file=%d memory=%d", manager.CountFileStorages, manager.CountMemoryStorages)
	}
	// Turning off a file policy's memory tier releases its share.
	manager.storageMap[2].(*FileStorage).memoryStorage = nil
	manager.refreshStorageCounts()
	if manager.memoryStorageCount.Load() != 1 {
		t.Fatal("disabled memory tier retained its payload share")
	}
}
