package requests

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// MaxInspectionBodyBytes bounds both wire and decoded buffers per body inspection.
// Uploads that do not require body inspection retain their configured streaming limit.
const MaxInspectionBodyBytes int64 = 8 << 20
const MaxInspectionBodyDuration = 15 * time.Second

type InspectionError struct {
	Err error
}

func (err *InspectionError) Error() string { return "body inspection failed: " + err.Err.Error() }
func (err *InspectionError) Unwrap() error { return err.Err }

func BodyInspectionError(err error) error {
	if err == nil {
		return nil
	}
	return &InspectionError{Err: err}
}

func inspectionLimit(max int64) int64 {
	if max <= 0 || max > MaxInspectionBodyBytes {
		return MaxInspectionBodyBytes
	}
	return max
}

// ReadInspectionBody checks an entire entity; exceeding the budget is an error,
// never a successful inspection of a truncated prefix.
func ReadInspectionBody(reader io.Reader, max int64) ([]byte, error) {
	max = inspectionLimit(max)
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, BodyInspectionError(err)
	}
	if int64(len(data)) > max {
		return nil, BodyInspectionError(&http.MaxBytesError{Limit: max})
	}
	return data, nil
}

// InspectRequestBody returns decoded bytes for rules and restores the original
// wire entity for forwarding, including its original Close ownership.
func InspectRequestBody(req *http.Request, max int64) ([]byte, error) {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.ContentLength > inspectionLimit(max) {
		return nil, BodyInspectionError(&http.MaxBytesError{Limit: inspectionLimit(max)})
	}
	data, replay, err := inspectBody(req.Body, req.Header.Get("Content-Encoding"), max)
	req.Body = replay
	return data, err
}

// InspectResponseBody keeps the origin's encoded entity intact after inspection.
func InspectResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return nil, nil
	}
	if resp.ContentLength > MaxInspectionBodyBytes {
		return nil, BodyInspectionError(&http.MaxBytesError{Limit: MaxInspectionBodyBytes})
	}
	data, replay, err := inspectBody(resp.Body, resp.Header.Get("Content-Encoding"), MaxInspectionBodyBytes)
	resp.Body = replay
	return data, err
}

func inspectBody(body io.ReadCloser, encoding string, max int64) ([]byte, io.ReadCloser, error) {
	return inspectBodyWithTimeout(body, encoding, max, MaxInspectionBodyDuration)
}

func inspectBodyWithTimeout(body io.ReadCloser, encoding string, max int64, timeout time.Duration) (data []byte, replay io.ReadCloser, resultErr error) {
	body = newReplayBody(nil, body)
	// Close an outstanding origin response read at the inspection deadline. Node
	// request readers additionally set a transport read deadline before entering.
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		_ = body.Close()
	})
	defer func() {
		if !timer.Stop() || timedOut.Load() {
			data = nil
			resultErr = BodyInspectionError(context.DeadlineExceeded)
		}
	}()
	max = inspectionLimit(max)
	wire, err := io.ReadAll(io.LimitReader(body, max+1))
	replay = newReplayBody(wire, body)
	if err != nil {
		return nil, replay, BodyInspectionError(err)
	}
	if int64(len(wire)) > max {
		return nil, replay, BodyInspectionError(&http.MaxBytesError{Limit: max})
	}
	var reader io.Reader = bytes.NewReader(wire)
	var closeDecoder func()
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return wire, replay, nil
	case "gzip", "x-gzip":
		decoder, decodeErr := gzip.NewReader(reader)
		if decodeErr != nil {
			return nil, replay, BodyInspectionError(decodeErr)
		}
		reader = decoder
		closeDecoder = func() { _ = decoder.Close() }
	case "deflate":
		decoder, decodeErr := zlib.NewReader(reader)
		if decodeErr != nil {
			decoder = flate.NewReader(bytes.NewReader(wire))
		}
		reader = decoder
		closeDecoder = func() { _ = decoder.Close() }
	case "br":
		reader = brotli.NewReader(reader)
	case "zstd":
		decoder, decodeErr := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(MaxInspectionBodyBytes)), zstd.WithDecoderMaxWindow(uint64(MaxInspectionBodyBytes)))
		if decodeErr != nil {
			return nil, replay, BodyInspectionError(decodeErr)
		}
		reader = decoder
		closeDecoder = decoder.Close
	default:
		return nil, replay, BodyInspectionError(fmt.Errorf("unsupported content encoding %q", encoding))
	}
	if closeDecoder != nil {
		defer closeDecoder()
	}
	decoded, err := ReadInspectionBody(reader, max)
	return decoded, replay, err
}

type replayBody struct {
	io.Reader
	owner    io.ReadCloser
	once     sync.Once
	closeErr error
}

func newReplayBody(prefix []byte, body io.ReadCloser) *replayBody {
	return &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix), body), owner: body}
}

func (body *replayBody) Close() error {
	body.once.Do(func() { body.closeErr = body.owner.Close() })
	return body.closeErr
}

func (body *replayBody) BodyLimit() int64 {
	if limited, ok := body.owner.(interface{ BodyLimit() int64 }); ok {
		return limited.BodyLimit()
	}
	return 0
}

// RestoreRequestBody remains compatible with checkpoints that explicitly restore
// a read: InspectRequestBody already supplies a replay of the original bytes.
func RestoreRequestBody(req *http.Request, data []byte) {
	if req == nil || len(data) == 0 {
		return
	}
	if _, restored := req.Body.(*replayBody); restored {
		return
	}
	if req.Body == nil {
		req.Body = io.NopCloser(bytes.NewReader(data))
		return
	}
	req.Body = newReplayBody(data, req.Body)
}

func IsBodyLimitError(err error) bool {
	var limitErr *http.MaxBytesError
	return errors.As(err, &limitErr)
}

// PostFormValue parses a bounded decoded copy rather than consuming the entity
// that later WAF rules and the origin must still receive.
func PostFormValue(req Request, key string) (string, error) {
	data := req.WAFGetCacheBody()
	if len(data) == 0 {
		var err error
		data, err = req.WAFReadBody(req.WAFMaxRequestSize())
		if err != nil {
			return "", err
		}
		req.WAFSetCacheBody(data)
		req.WAFRestoreBody(data)
	}
	raw := req.WAFRaw()
	parsed := raw.Clone(raw.Context())
	parsed.Header = raw.Header.Clone()
	parsed.Header.Del("Content-Encoding")
	parsed.ContentLength = int64(len(data))
	parsed.Body = io.NopCloser(bytes.NewReader(data))
	defer parsed.Body.Close()
	parsed.Form, parsed.PostForm, parsed.MultipartForm = nil, nil, nil
	err := parsed.ParseMultipartForm(inspectionLimit(req.WAFMaxRequestSize()))
	if parsed.MultipartForm != nil {
		defer parsed.MultipartForm.RemoveAll()
	}
	if err != nil && !errors.Is(err, http.ErrNotMultipart) {
		return "", BodyInspectionError(err)
	}
	return parsed.PostForm.Get(key), nil
}
