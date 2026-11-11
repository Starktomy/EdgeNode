// Copyright 2022 GoEdge goedge.cdn@gmail.com. All rights reserved.

package compressions

import (
	"io"
	"sync/atomic"
)

type BaseReader struct {
	pool *ReaderPool
	// Gzip/deflate/brotli retain this adapter, not the origin body/request.
	parent io.Reader
	bytes  byteReaderInput

	isFinished bool
	hits       uint32
}

func (this *BaseReader) SetPool(pool *ReaderPool) {
	this.pool = pool
}

func (this *BaseReader) Finish(obj Reader) error {
	if this.isFinished {
		return nil
	}
	err := obj.RawClose()
	this.isFinished = true
	this.parent = nil
	if err == nil && this.pool != nil {
		// Put transfers ownership; do not access the object after publication.
		this.pool.Put(obj)
	}
	return err
}

func (this *BaseReader) Read(p []byte) (int, error) {
	if this.parent == nil {
		return 0, io.EOF
	}
	return this.parent.Read(p)
}

// Preserve ByteReader only when the current parent provides it. Deflate/gzip
// otherwise add buffering and can consume bytes past a compressed member.
// Ordinary HTTP bodies must keep their normal buffered Read path.
func (this *BaseReader) input() io.Reader {
	if _, ok := this.parent.(io.ByteReader); ok {
		this.bytes.owner = this
		return &this.bytes
	}
	return this
}

type byteReaderInput struct{ owner *BaseReader }

func (r *byteReaderInput) Read(p []byte) (int, error) { return r.owner.Read(p) }

func (r *byteReaderInput) ReadByte() (byte, error) {
	if r.owner.parent == nil {
		return 0, io.EOF
	}
	return r.owner.parent.(io.ByteReader).ReadByte()
}

func (this *BaseReader) ResetFinish() {
	this.isFinished = false
}

func (this *BaseReader) IncreaseHit() uint32 {
	return atomic.AddUint32(&this.hits, 1)
}
