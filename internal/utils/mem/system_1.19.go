// Copyright 2023 GoEdge goedge.cdn@gmail.com. All rights reserved. Official site: https://goedge.cloud .
//go:build go1.19

package memutils

import (
	"math"
	"os"
	"runtime/debug"
	"sync"
)

type memoryLimitPolicy struct {
	mu          sync.Mutex
	applied     bool
	disabled    bool
	lastApplied int64
}

var defaultMemoryLimit memoryLimitPolicy

// 设置 Go 管理内存的软限制；CGO、mmap、内核缓存等不在此限制内。
// 这不是 RSS 上限，应用工作集过大时 Go 仍可能超过该值。
func setMaxMemory(memoryBytes uint64) {
	defaultMemoryLimit.apply(memoryBytes, os.Getenv("GOMEMLIMIT"), debug.SetMemoryLimit)
}

func (p *memoryLimitPolicy) apply(memoryBytes uint64, env string, setLimit func(int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.disabled {
		return
	}
	if env != "" {
		// Includes GOMEMLIMIT=off: an explicit operator setting takes priority.
		p.disabled = true
		return
	}
	if memoryBytes == 0 {
		return
	}

	current := setLimit(-1)
	if (!p.applied && current != math.MaxInt64) || (p.applied && current != p.lastApplied) {
		// Respect limits installed by another caller, including after startup.
		p.disabled = true
		return
	}

	// Divide first to avoid overflowing for exceptionally large host totals.
	target := memoryBytes/5*4 + memoryBytes%5*4/5
	if target > math.MaxInt64 {
		target = math.MaxInt64
	}
	if target == 0 {
		return
	}
	if int64(target) != current {
		setLimit(int64(target))
	}
	p.applied = true
	p.lastApplied = int64(target)
}
