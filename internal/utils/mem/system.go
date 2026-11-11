// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.

package memutils

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	teaconst "github.com/TeaOSLab/EdgeNode/internal/const"
	"github.com/TeaOSLab/EdgeNode/internal/utils/goman"
	"github.com/shirou/gopsutil/v3/mem"
)

type memorySnapshot struct {
	totalBytes     uint64
	availableBytes uint64
}

// Readers only load an immutable snapshot; procfs/cgroup reads run at startup
// and on the background timer, never on a request's cached getter path.
type memoryMonitor struct {
	once       sync.Once
	state      atomic.Pointer[memorySnapshot]
	refreshMu  sync.Mutex
	readMemory func() (memorySnapshot, error)
	applyLimit func(uint64)
}

var systemMemory = memoryMonitor{
	readMemory: readSystemMemory,
	applyLimit: setMaxMemory,
}

func (m *memoryMonitor) snapshot() *memorySnapshot {
	m.once.Do(m.refresh)
	return m.state.Load()
}

func (m *memoryMonitor) refresh() {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	stat, err := m.readMemory()
	if err != nil || stat.totalBytes == 0 {
		// Retain a previous valid budget on transient host-stat failures. An
		// unknown initial budget must not set a zero-byte Go memory limit.
		if m.state.Load() == nil {
			m.state.Store(&memorySnapshot{})
		}
		return
	}
	if stat.availableBytes > stat.totalBytes {
		stat.availableBytes = stat.totalBytes
	}
	if m.applyLimit != nil {
		m.applyLimit(stat.totalBytes)
	}
	m.state.Store(&stat)
}

func readSystemMemory() (memorySnapshot, error) {
	stat, err := mem.VirtualMemory()
	if err != nil {
		return memorySnapshot{}, err
	}
	if stat.Total == 0 {
		return memorySnapshot{}, fmt.Errorf("system memory total is unavailable")
	}
	return effectiveMemory(memorySnapshot{
		totalBytes:     stat.Total,
		availableBytes: stat.Available,
	}), nil
}

func init() {
	if !teaconst.IsMain {
		return
	}

	_ = SystemMemoryGB()

	goman.New(func() {
		var ticker = time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			systemMemory.refresh()
		}
	})
}

// SystemMemoryGB 返回有效内存的整 GiB 数，兼容旧调用方，必须不小于 1。
// 小于 1 GiB 的精确资源预算请使用 SystemMemoryBytes。
func SystemMemoryGB() int {
	return memoryGB(systemMemory.snapshot().totalBytes)
}

func memoryGB(totalBytes uint64) int {
	if totalBytes < 1<<30 {
		return 1
	}
	return int(totalBytes >> 30)
}

// SystemMemoryBytes 返回宿主机与可见 cgroup 限额中的最小有效字节数。
// 无法初始化系统内存信息时返回 0，调用方应使用保守的回退预算。
func SystemMemoryBytes() uint64 {
	return systemMemory.snapshot().totalBytes
}

// AvailableMemoryGB 返回最近快照中的可用整 GiB 数，可以为 0。
func AvailableMemoryGB() int {
	return int(systemMemory.snapshot().availableBytes >> 30)
}
