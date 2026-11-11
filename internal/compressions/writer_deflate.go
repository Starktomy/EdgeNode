// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.

package compressions

import (
	"compress/flate"
	"io"
)

type DeflateWriter struct {
	BaseWriter

	writer *flate.Writer
	level  int
}

func NewDeflateWriter(writer io.Writer, level int) (Writer, error) {
	return sharedDeflateWriterPool.Get(writer, level)
}

func newDeflateWriter(writer io.Writer) (Writer, error) {
	var level = GenerateCompressLevel(flate.BestSpeed, flate.BestCompression)

	result := &DeflateWriter{BaseWriter: BaseWriter{parent: writer}, level: level}
	flateWriter, err := flate.NewWriter(&result.BaseWriter, level)
	if err != nil {
		return nil, err
	}

	result.writer = flateWriter
	return result, nil
}

func (this *DeflateWriter) Write(p []byte) (int, error) {
	return this.writer.Write(p)
}

func (this *DeflateWriter) Flush() error {
	return this.writer.Flush()
}

func (this *DeflateWriter) Reset(writer io.Writer) {
	this.parent = writer
	this.writer.Reset(&this.BaseWriter)
}

func (this *DeflateWriter) RawClose() error {
	return this.writer.Close()
}

func (this *DeflateWriter) Close() error {
	return this.Finish(this)
}

func (this *DeflateWriter) Level() int {
	return this.level
}
