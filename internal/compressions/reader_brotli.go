// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.
//go:build !plus || !linux

package compressions

import (
	"io"
	"strings"

	"github.com/andybalholm/brotli"
)

type BrotliReader struct {
	BaseReader

	reader *brotli.Reader
}

func NewBrotliReader(reader io.Reader) (Reader, error) {
	return sharedBrotliReaderPool.Get(reader)
}

func newBrotliReader(reader io.Reader) (Reader, error) {
	result := &BrotliReader{BaseReader: BaseReader{parent: reader}}
	result.reader = brotli.NewReader(result.input())
	return result, nil
}

func (this *BrotliReader) Read(p []byte) (n int, err error) {
	n, err = this.reader.Read(p)
	if err != nil && strings.Contains(err.Error(), "excessive") {
		err = io.EOF
	}
	return
}

func (this *BrotliReader) Reset(reader io.Reader) error {
	this.parent = reader
	return this.reader.Reset(this.input())
}

func (this *BrotliReader) RawClose() error {
	return nil
}

func (this *BrotliReader) Close() error {
	return this.Finish(this)
}
