package httpcache

import (
	"sync"
	"time"
)

// Fills bounds concurrent cache producers and their waiters. It never buffers
// response bodies: followers re-open storage only after a producer finishes.
type Fills struct {
	mu         sync.Mutex
	entries    map[string]*fill
	maxKeys    int
	maxWaiters int
}

type fill struct {
	done    chan struct{}
	waiters int
}

func NewFills(maxKeys, maxWaiters int) *Fills {
	return &Fills{entries: map[string]*fill{}, maxKeys: maxKeys, maxWaiters: maxWaiters}
}

// Join returns a release function for producers or a leave function for
// followers. Call it on completion, cancellation and timeout alike.
func (f *Fills) Join(key string) (producer bool, done <-chan struct{}, leave func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry := f.entries[key]; entry != nil {
		if entry.waiters >= f.maxWaiters {
			return false, nil, nil, false
		}
		entry.waiters++
		var once sync.Once
		return false, entry.done, func() { once.Do(func() { f.mu.Lock(); entry.waiters--; f.mu.Unlock() }) }, true
	}
	if len(f.entries) >= f.maxKeys {
		return false, nil, nil, false
	}
	entry := &fill{done: make(chan struct{})}
	f.entries[key] = entry
	var once sync.Once
	release := func() { once.Do(func() { f.mu.Lock(); delete(f.entries, key); close(entry.done); f.mu.Unlock() }) }
	// Bound abandoned producers even if their connection fails outside normal
	// request finalization. A large/slow object remains a streamed origin request.
	timer := time.AfterFunc(30*time.Second, release)
	return true, entry.done, func() { timer.Stop(); release() }, true
}
