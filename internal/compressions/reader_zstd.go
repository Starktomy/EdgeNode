// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.

package compressions

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

type ZSTDReader struct {
	BaseReader

	reader *zstd.Decoder
}

func NewZSTDReader(reader io.Reader) (Reader, error) {
	return sharedZSTDReaderPool.Get(reader)
}

func newZSTDReader(reader io.Reader) (Reader, error) {
	r, err := zstd.NewReader(reader, zstd.WithDecoderMaxWindow(256<<20))
	if err != nil {
		return nil, err
	}
	return &ZSTDReader{
		reader: r,
	}, nil
}

func (this *ZSTDReader) Read(p []byte) (n int, err error) {
	if this.reader == nil {
		return 0, zstd.ErrDecoderClosed
	}
	return this.reader.Read(p)
}

func (this *ZSTDReader) Reset(reader io.Reader) error {
	if this.reader == nil {
		return zstd.ErrDecoderClosed
	}
	return this.reader.Reset(reader)
}

func (this *ZSTDReader) RawClose() error {
	if this.reader != nil {
		this.reader.Close()
		this.reader = nil
	}
	return nil
}

func (this *ZSTDReader) Close() error {
	// Decoder.Close destroys its resources and Reset cannot revive it. Do not
	// cache a closed decoder only to discard/recreate it at the next Get.
	this.pool = nil
	return this.Finish(this)
}
