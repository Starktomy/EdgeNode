// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.

package compressions

import (
	"compress/flate"
	"io"
)

type DeflateReader struct {
	BaseReader

	reader io.ReadCloser
}

func NewDeflateReader(reader io.Reader) (Reader, error) {
	return sharedDeflateReaderPool.Get(reader)
}

func newDeflateReader(reader io.Reader) (Reader, error) {
	result := &DeflateReader{BaseReader: BaseReader{parent: reader}}
	result.reader = flate.NewReader(result.input())
	return result, nil
}

func (this *DeflateReader) Read(p []byte) (n int, err error) {
	return this.reader.Read(p)
}

func (this *DeflateReader) Reset(reader io.Reader) error {
	this.parent = reader
	// NewReader guarantees Resetter. Keep its buffers across pooled uses;
	// constructing another decoder here allocated about 40 KiB per request.
	return this.reader.(flate.Resetter).Reset(this.input(), nil)
}

func (this *DeflateReader) RawClose() error {
	return this.reader.Close()
}

func (this *DeflateReader) Close() error {
	return this.Finish(this)
}
