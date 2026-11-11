package compressions

import (
	"bytes"
	"io"
	"testing"
)

// Input and codec warmup are outside the timer. Count actual Write/Close/Get
// work, including checks and resets; no filesystem or request generator cost.
func BenchmarkCompressionPoolReuse(b *testing.B) {
	data := bytes.Repeat([]byte("compressor reuse fixture data\n"), 1024)
	for _, name := range securityCodecNames {
		b.Run(name, func(b *testing.B) {
			pool := securityWriterPool(name, 1)
			writer, err := pool.Get(io.Discard, 1)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				b.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				writer, err := pool.Get(io.Discard, 1)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := writer.Write(data); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCompressionReaderPoolReuse(b *testing.B) {
	data := bytes.Repeat([]byte("compressor reuse fixture data\n"), 1024)
	for _, name := range securityCodecNames {
		b.Run(name, func(b *testing.B) {
			writers := securityWriterPool(name, 1)
			var encoded bytes.Buffer
			writer, err := writers.Get(&encoded, 1)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				b.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				b.Fatal(err)
			}
			pool := securityReaderPool(name, 1)
			parent := bytes.NewReader(encoded.Bytes())
			reader, err := pool.Get(parent)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				b.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				parent.Reset(encoded.Bytes())
				reader, err := pool.Get(parent)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, reader); err != nil {
					b.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
