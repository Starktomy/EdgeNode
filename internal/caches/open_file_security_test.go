package caches

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"

	fsutils "github.com/TeaOSLab/EdgeNode/internal/utils/fs"
)

func securityCacheFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func assertCacheFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("cache retained rejected descriptor %s: %v", file.Name(), err)
	}
}

func TestSecurityFileReaderCloseReleasesRejectedFiles(t *testing.T) {
	for _, name := range []string{"oversized", "capacity", "count", "closed"} {
		t.Run(name, func(t *testing.T) {
			cache, err := NewOpenFileCache(1)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.CloseAll()
			cache.SetCapacity(1 << 20)
			var size int64 = 1
			var previous *os.File
			switch name {
			case "oversized":
				// The logical cached size exercises the guard without writing
				// a large fixture or allocating a large body.
				size = maxOpenFileSize + 1
			case "capacity":
				cache.SetCapacity(1)
			case "count":
				previous = securityCacheFile(t)
				cache.Put(previous.Name(), NewOpenFile(previous, nil, nil, 0, 1))
			case "closed":
				cache.CloseAll()
			}
			file := securityCacheFile(t)
			reader := NewFileReader(fsutils.NewFile(file, fsutils.FlagRead))
			reader.openFileCache = cache
			reader.bodySize = size
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			assertCacheFileClosed(t, file)
			if previous != nil {
				assertCacheFileClosed(t, previous)
			}
		})
	}
}

func TestSecurityOpenFilePoolConcurrentOwnershipAndClose(t *testing.T) {
	pool := NewOpenFilePool("pool")
	var files []*os.File
	for i := 0; i < 32; i++ {
		file := securityCacheFile(t)
		files = append(files, file)
		if !pool.Put(NewOpenFile(file, nil, nil, 0, 1)) {
			t.Fatal("could not seed pool")
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	closing := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 512; i++ {
				if file, _, _ := pool.Get(); file != nil {
					if _, err := file.fp.Stat(); err != nil {
						t.Error("pool closed a file while its reader owned it")
					}
					pool.Put(file)
				}
				_ = pool.TotalSize()
				_ = pool.Len()
				if worker == 0 && i == 128 {
					close(closing)
				}
				runtime.Gosched()
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-closing
		pool.SetClosing()
		pool.Close()
	}()
	close(start)
	wg.Wait()
	pool.Close()
	if pool.Len() != 0 || pool.TotalSize() != 0 {
		t.Fatalf("closed pool retained entries: count=%d size=%d", pool.Len(), pool.TotalSize())
	}
	for _, file := range files {
		assertCacheFileClosed(t, file)
	}
}

func TestSecurityOpenFileCacheShutdownPreservesCheckedOutReader(t *testing.T) {
	cache, err := NewOpenFileCache(4)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.CloseAll()
	cache.SetCapacity(1 << 20)
	file := securityCacheFile(t)
	cache.Put(file.Name(), NewOpenFile(file, nil, nil, 0, 1))
	entry := cache.Get(file.Name())
	if entry == nil {
		t.Fatal("could not check out cached file")
	}
	cache.CloseAll()
	if _, err := file.Stat(); err != nil {
		t.Fatal("shutdown closed a descriptor still owned by a reader")
	}
	reader := NewFileReader(fsutils.NewFile(file, fsutils.FlagRead))
	reader.openFileCache = cache
	reader.openFile = entry
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	assertCacheFileClosed(t, file)
}

func TestSecurityOpenFileCacheConcurrentGetPutAndShutdown(t *testing.T) {
	cache, err := NewOpenFileCache(64)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.CloseAll()
	cache.SetCapacity(1 << 20)
	var files []*os.File
	for i := 0; i < 32; i++ {
		file := securityCacheFile(t)
		files = append(files, file)
		cache.Put(file.Name(), NewOpenFile(file, nil, nil, 0, 1))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	closing := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 512; i++ {
				filename := files[(i+worker)%len(files)].Name()
				if file := cache.Get(filename); file != nil {
					cache.Put(filename, file)
				}
				if i%32 == 0 {
					cache.SetCapacity(1 << 20)
				}
				if worker == 0 && i == 128 {
					close(closing)
				}
				runtime.Gosched()
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-closing
		for _, file := range files[:16] {
			cache.Close(file.Name())
		}
		cache.CloseAll()
	}()
	close(start)
	wg.Wait()
	cache.CloseAll()
	cache.locker.RLock()
	count, size := cache.count, cache.usedSize
	cache.locker.RUnlock()
	if count != 0 || size != 0 {
		t.Fatalf("closed cache retained accounting: count=%d size=%d", count, size)
	}
	for _, file := range files {
		assertCacheFileClosed(t, file)
	}
}
