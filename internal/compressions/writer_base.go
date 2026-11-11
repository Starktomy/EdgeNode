// Copyright 2022 GoEdge goedge.cdn@gmail.com. All rights reserved.

package compressions

import (
	"io"
	"sync/atomic"
)

type BaseWriter struct {
	pool *WriterPool
	// The codec retains this stable adapter instead of a request's output
	// chain. Clear the parent at Close without resetting codec state twice.
	parent io.Writer

	isFinished bool

	hits uint32
}

func (this *BaseWriter) SetPool(pool *WriterPool) {
	this.pool = pool
}

func (this *BaseWriter) Finish(obj Writer) error {
	if this.isFinished {
		return nil
	}
	err := obj.RawClose()
	this.isFinished = true
	this.parent = nil
	if err == nil && this.pool != nil {
		// Put transfers ownership. Never touch this object after publication;
		// another request may already have reset and started using it.
		this.pool.Put(obj)
	}
	return err
}

func (this *BaseWriter) Write(p []byte) (int, error) {
	if this.parent == nil {
		return 0, io.ErrClosedPipe
	}
	return this.parent.Write(p)
}

func (this *BaseWriter) ResetFinish() {
	this.isFinished = false
}

func (this *BaseWriter) IncreaseHit() uint32 {
	return atomic.AddUint32(&this.hits, 1)
}
