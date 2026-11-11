package compressions

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"weak"
)

var securityCodecNames = []string{"gzip", "deflate", "brotli", "zstd"}

func securityWriterPool(name string, capacity int) *WriterPool {
	return NewWriterPool(capacity, func(parent io.Writer, _ int) (Writer, error) {
		switch name {
		case "gzip":
			return newGzipWriter(parent)
		case "deflate":
			return newDeflateWriter(parent)
		case "brotli":
			return newBrotliWriter(parent)
		default:
			return newZSTDWriter(parent)
		}
	})
}

func securityReaderPool(name string, capacity int) *ReaderPool {
	return NewReaderPool(capacity, func(parent io.Reader) (Reader, error) {
		switch name {
		case "gzip":
			return newGzipReader(parent)
		case "deflate":
			return newDeflateReader(parent)
		case "brotli":
			return newBrotliReader(parent)
		default:
			return newZSTDReader(parent)
		}
	})
}

type securityOutput struct {
	bytes.Buffer
	retained []byte
	closed   int
}

func (p *securityOutput) Close() error { p.closed++; return nil }

type securityInput struct {
	*bytes.Reader
	retained []byte
	closed   int
}

func (p *securityInput) Close() error { p.closed++; return nil }

func TestSecurityCompressionPoolRoundTrips(t *testing.T) {
	for _, name := range securityCodecNames {
		t.Run(name, func(t *testing.T) {
			writers := securityWriterPool(name, 1)
			readers := securityReaderPool(name, 1)
			for _, size := range []int{0, 1, 1024, 32 << 10, 3, 64 << 10, 0} {
				data := bytes.Repeat([]byte("pool reuse roundtrip\n"), size/21+1)[:size]
				var output securityOutput
				writer, err := writers.Get(&output, 1)
				if err != nil {
					t.Fatal(err)
				}
				middle := len(data) / 2
				if _, err := writer.Write(data[:middle]); err != nil {
					t.Fatal(err)
				}
				if err := writer.Flush(); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(data[middle:]); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				// No new borrower exists yet: repeated Close must remain harmless.
				if err := writer.Close(); err != nil || output.closed != 0 {
					t.Fatal("codec closed its parent or repeated Close failed", err)
				}
				input := &securityInput{Reader: bytes.NewReader(output.Bytes())}
				reader, err := readers.Get(input)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(reader)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("roundtrip size=%d: got=%d error=%v", size, len(got), err)
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
				if err := reader.Close(); err != nil || input.closed != 0 {
					t.Fatal("codec closed the HTTP body or repeated Close failed", err)
				}
				if name == "zstd" && len(readers.c) != 0 {
					t.Fatal("destroyed zstd decoder was returned for reuse")
				}
			}
		})
	}
}

//go:noinline
func securityCloseWriterWithParent(t *testing.T, pool *WriterPool) weak.Pointer[securityOutput] {
	parent := &securityOutput{retained: make([]byte, 1<<20)}
	reference := weak.Make(parent)
	writer, err := pool.Get(parent, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("retention fixture")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return reference
}

//go:noinline
func securityCloseReaderWithParent(t *testing.T, pool *ReaderPool, data []byte) weak.Pointer[securityInput] {
	parent := &securityInput{Reader: bytes.NewReader(data), retained: make([]byte, 1<<20)}
	reference := weak.Make(parent)
	reader, err := pool.Get(parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestSecurityCompressionPoolsReleaseRequestParents(t *testing.T) {
	for _, name := range securityCodecNames {
		t.Run(name, func(t *testing.T) {
			writers := securityWriterPool(name, 1)
			writerParent := securityCloseWriterWithParent(t, writers)
			// Check before borrowing again: a later Get/Reset would replace the
			// parent and hide retention by an otherwise idle pooled writer.
			runtime.GC()
			runtime.GC()
			if writerParent.Value() != nil {
				t.Fatal("idle writer pool retained a completed request")
			}
			runtime.KeepAlive(writers)
			var encoded bytes.Buffer
			writer, err := writers.Get(&encoded, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("input retention fixture")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			readers := securityReaderPool(name, 1)
			readerParent := securityCloseReaderWithParent(t, readers, encoded.Bytes())
			runtime.GC()
			runtime.GC()
			if readerParent.Value() != nil {
				t.Fatal("closed reader retained an origin body/request")
			}
			runtime.KeepAlive(writers)
			runtime.KeepAlive(readers)
		})
	}
}

type securityFailOutput struct{ retained []byte }

func (*securityFailOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

//go:noinline
func securityFailWriterWithParent(t *testing.T, pool *WriterPool) weak.Pointer[securityFailOutput] {
	parent := &securityFailOutput{retained: make([]byte, 1<<20)}
	reference := weak.Make(parent)
	writer, err := pool.Get(parent, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("failed response output"))
	if err := writer.Close(); err == nil {
		t.Fatal("codec hid an output failure")
	}
	if len(pool.c) != 0 {
		t.Fatal("failed codec entered the reusable pool")
	}
	return reference
}

func TestSecurityCompressionFailureReleasesParent(t *testing.T) {
	for _, name := range securityCodecNames {
		t.Run(name, func(t *testing.T) {
			pool := securityWriterPool(name, 1)
			reference := securityFailWriterWithParent(t, pool)
			runtime.GC()
			runtime.GC()
			if reference.Value() != nil {
				t.Fatal("failed codec retained a request parent")
			}
			runtime.KeepAlive(pool)
		})
	}
}

type securityPlainInput struct {
	input   *bytes.Reader
	maxRead int
}

func (r *securityPlainInput) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	return r.input.Read(p)
}

func securityEncode(t *testing.T, pool *WriterPool, data []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer, err := pool.Get(&encoded, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestSecurityCompressionDeflateByteReaderBoundarySwitch(t *testing.T) {
	writers, readers := securityWriterPool("deflate", 1), securityReaderPool("deflate", 1)
	data, trailer := []byte("first-member"), []byte("trailing-frame")
	encoded := securityEncode(t, writers, data)
	for _, supportsBytes := range []bool{true, false, true, false, true} {
		input := bytes.NewReader(append(append([]byte(nil), encoded...), trailer...))
		plain := &securityPlainInput{input: input}
		var parent io.Reader = plain
		if supportsBytes {
			parent = input
		}
		reader, err := readers.Get(parent)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("ByteReader=%v decoded=%q error=%v", supportsBytes, got, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if supportsBytes {
			rest, err := io.ReadAll(input)
			if err != nil || !bytes.Equal(rest, trailer) {
				t.Fatalf("ByteReader lost the next member/frame: %q, %v", rest, err)
			}
		} else if plain.maxRead <= 1 {
			t.Fatal("ordinary HTTP reader degraded into one-byte reads")
		}
	}
}

func TestSecurityCompressionGzipConcatenatedMembersAndInputSwitch(t *testing.T) {
	writers, readers := securityWriterPool("gzip", 1), securityReaderPool("gzip", 1)
	first := securityEncode(t, writers, []byte("first-member"))
	second := securityEncode(t, writers, []byte("second-member"))
	encoded := append(first, second...)
	for _, supportsBytes := range []bool{true, false, true, false, true} {
		input := bytes.NewReader(encoded)
		plain := &securityPlainInput{input: input}
		var parent io.Reader = plain
		if supportsBytes {
			parent = input
		}
		reader, err := readers.Get(parent)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != "first-membersecond-member" {
			t.Fatalf("ByteReader=%v changed concatenated gzip members: %q, %v", supportsBytes, got, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if !supportsBytes && plain.maxRead <= 1 {
			t.Fatal("ordinary HTTP reader degraded into one-byte reads")
		}
	}
}

type securityReuseWriter struct {
	BaseWriter
	closed *atomic.Int64
}

func (*securityReuseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*securityReuseWriter) Flush() error                { return nil }
func (*securityReuseWriter) Reset(io.Writer)             {}
func (w *securityReuseWriter) RawClose() error           { w.closed.Add(1); return nil }
func (w *securityReuseWriter) Close() error              { return w.Finish(w) }
func (*securityReuseWriter) Level() int                  { return 1 }

type securityReuseReader struct {
	BaseReader
	closed *atomic.Int64
}

func (*securityReuseReader) Read([]byte) (int, error) { return 0, io.EOF }
func (*securityReuseReader) Reset(io.Reader) error    { return nil }
func (r *securityReuseReader) RawClose() error        { r.closed.Add(1); return nil }
func (r *securityReuseReader) Close() error           { return r.Finish(r) }

func TestSecurityCompressionPoolConcurrentHandoff(t *testing.T) {
	const workers, iterations = 8, 2000
	var writerCloses, readerCloses atomic.Int64
	writers := NewWriterPool(workers, func(io.Writer, int) (Writer, error) {
		return &securityReuseWriter{closed: &writerCloses}, nil
	})
	readers := NewReaderPool(workers, func(io.Reader) (Reader, error) {
		return &securityReuseReader{closed: &readerCloses}, nil
	})
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for range iterations {
				writer, err := writers.Get(io.Discard, 1)
				if err != nil {
					t.Error(err)
					return
				}
				if err := writer.Close(); err != nil {
					t.Error(err)
				}
				reader, err := readers.Get(bytes.NewReader(nil))
				if err != nil {
					t.Error(err)
					return
				}
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	group.Wait()
	if writerCloses.Load() != workers*iterations || readerCloses.Load() != workers*iterations {
		t.Fatalf("a stale Finish skipped a new borrower's close: writers=%d readers=%d", writerCloses.Load(), readerCloses.Load())
	}
}

func TestSecurityCompressionBusyConcurrentAccess(t *testing.T) {
	pool := NewWriterPool(1, func(io.Writer, int) (Writer, error) { return &securityReuseWriter{closed: new(atomic.Int64)}, nil })
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 2000 {
				_ = IsBusy()
				writer, err := pool.Get(io.Discard, 1)
				if errors.Is(err, ErrIsBusy) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				_ = writer.Close()
			}
		})
	}
	group.Wait()
	// Each test owns its local pool; leave no throttle for later codec tests.
	isBusy.Store(false)
}

func TestSecurityCompressionRealCodecConcurrentReuse(t *testing.T) {
	const workers, iterations = 4, 12
	for _, name := range securityCodecNames {
		t.Run(name, func(t *testing.T) {
			writers := securityWriterPool(name, workers)
			readers := securityReaderPool(name, workers)
			var group sync.WaitGroup
			for worker := range workers {
				group.Go(func() {
					for iteration := range iterations {
						data := bytes.Repeat([]byte{byte(worker), byte(iteration)}, 4096)
						var encoded bytes.Buffer
						writer, err := writers.Get(&encoded, 1)
						if err != nil {
							t.Error(err)
							return
						}
						if _, err := writer.Write(data); err != nil {
							t.Error(err)
							return
						}
						if err := writer.Close(); err != nil {
							t.Error(err)
							return
						}
						reader, err := readers.Get(bytes.NewReader(encoded.Bytes()))
						if err != nil {
							t.Error(err)
							return
						}
						got, err := io.ReadAll(reader)
						if err != nil || !bytes.Equal(got, data) {
							t.Error("compressed stream was truncated or crossed between requests", err)
						}
						if err := reader.Close(); err != nil {
							t.Error(err)
						}
					}
				})
			}
			group.Wait()
		})
	}
}
