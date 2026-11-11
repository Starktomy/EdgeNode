package requests

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type trackedBody struct {
	io.Reader
	closed int
}

func (body *trackedBody) Close() error { body.closed++; return nil }

type endlessBody struct{ read int }

func (body *endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	body.read += len(p)
	return len(p), nil
}

func TestSecurityInspectRequestUnknownLengthPreservesEntityAndClose(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			owner := &trackedBody{Reader: strings.NewReader("payload")}
			req := &http.Request{Method: method, Header: http.Header{}, ContentLength: -1, Body: owner}
			data, err := InspectRequestBody(req, 32)
			if err != nil || string(data) != "payload" {
				t.Fatalf("inspect = %q, %v", data, err)
			}
			RestoreRequestBody(req, data)
			forwarded, err := io.ReadAll(req.Body)
			if err != nil || string(forwarded) != "payload" {
				t.Fatalf("forwarded = %q, %v", forwarded, err)
			}
			_ = req.Body.Close()
			_ = req.Body.Close()
			if owner.closed != 1 {
				t.Fatalf("original body closed %d times", owner.closed)
			}
		})
	}
}

func TestSecurityInspectCompressedRequestPreservesWireAndBoundsExpansion(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(`{"name":"suspicious-value"}`))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), compressed.Bytes()...)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(wire)), Header: http.Header{"Content-Encoding": {"gzip"}}, ContentLength: -1}
	decoded, err := InspectRequestBody(req, 1024)
	if err != nil || !bytes.Contains(decoded, []byte("suspicious-value")) {
		t.Fatalf("decoded = %q, %v", decoded, err)
	}
	RestoreRequestBody(req, decoded)
	forwarded, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(forwarded, wire) {
		t.Fatalf("wire entity changed: %v", err)
	}
	_ = req.Body.Close()

	compressed.Reset()
	writer = gzip.NewWriter(&compressed)
	_, _ = writer.Write(bytes.Repeat([]byte("x"), 4096))
	_ = writer.Close()
	req = &http.Request{Body: io.NopCloser(bytes.NewReader(compressed.Bytes())), Header: http.Header{"Content-Encoding": {"gzip"}}, ContentLength: -1}
	if _, err := InspectRequestBody(req, 128); !IsBodyLimitError(err) {
		t.Fatalf("expansion error = %v", err)
	}
	_ = req.Body.Close()
}

func TestSecurityInspectionBudgetRejectsTruncationAndEndlessBodies(t *testing.T) {
	reader := &endlessBody{}
	if _, err := ReadInspectionBody(reader, 1024); !IsBodyLimitError(err) {
		t.Fatalf("limit error = %v", err)
	}
	if reader.read != 1025 {
		t.Fatalf("read %d bytes, want budget+1", reader.read)
	}
	if _, err := ReadInspectionBody(strings.NewReader("exact"), 5); err != nil {
		t.Fatalf("exact boundary: %v", err)
	}

	req := &http.Request{Body: io.NopCloser(strings.NewReader("payload")), Header: http.Header{"Content-Encoding": {"gzip, br"}}, ContentLength: -1}
	_, err := InspectRequestBody(req, 32)
	var inspectionErr *InspectionError
	if !errors.As(err, &inspectionErr) {
		t.Fatalf("unsupported encoding error = %v", err)
	}
	_ = req.Body.Close()
}

func TestSecurityResponseInspectionBoundsUnknownLengthAndKeepsBody(t *testing.T) {
	owner := &trackedBody{Reader: strings.NewReader("origin data")}
	resp := &http.Response{Body: owner, ContentLength: -1, Header: http.Header{}}
	data, err := InspectResponseBody(resp)
	if err != nil || string(data) != "origin data" {
		t.Fatalf("response inspection = %q, %v", data, err)
	}
	forwarded, err := io.ReadAll(resp.Body)
	if err != nil || string(forwarded) != "origin data" {
		t.Fatalf("forward response = %q, %v", forwarded, err)
	}
	_ = resp.Body.Close()
	if owner.closed != 1 {
		t.Fatalf("origin body closed %d times", owner.closed)
	}

	reader := &endlessBody{}
	resp = &http.Response{Body: io.NopCloser(reader), ContentLength: -1, Header: http.Header{}}
	if _, err := InspectResponseBody(resp); !IsBodyLimitError(err) {
		t.Fatalf("response limit error = %v", err)
	}
	if int64(reader.read) != MaxInspectionBodyBytes+1 {
		t.Fatalf("read %d bytes", reader.read)
	}
	_ = resp.Body.Close()
}

func TestSecurityPostFormInspectionDoesNotConsumeOriginBody(t *testing.T) {
	raw := &http.Request{Method: http.MethodPost, Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, Body: io.NopCloser(strings.NewReader("user=alice&value=marker")), ContentLength: -1}
	req := NewTestRequest(raw)
	value, err := PostFormValue(req, "user")
	if err != nil || value != "alice" {
		t.Fatalf("form value = %q, %v", value, err)
	}
	forwarded, err := io.ReadAll(raw.Body)
	if err != nil || string(forwarded) != "user=alice&value=marker" {
		t.Fatalf("CC inspection consumed origin body: %q, %v", forwarded, err)
	}
	_ = raw.Body.Close()
}

func TestSecurityInspectionDeadlineInterruptsSlowBody(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	_, replay, err := inspectBodyWithTimeout(reader, "", 1024, 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow body deadline error = %v", err)
	}
	_ = replay.Close()
}
