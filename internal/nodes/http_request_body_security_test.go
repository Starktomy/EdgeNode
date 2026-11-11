package nodes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/shared"
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
)

type securityUploadBody struct {
	io.Reader
	closed int
}

func (body *securityUploadBody) Close() error { body.closed++; return nil }

func securityBodyRequest(t *testing.T, content string, declared int64) (*HTTPRequest, *securityUploadBody) {
	t.Helper()
	limit := &serverconfigs.HTTPRequestLimitConfig{IsOn: true, MaxBodySize: &shared.SizeCapacity{Count: 8, Unit: shared.SizeCapacityUnitByte}}
	if err := limit.Init(); err != nil {
		t.Fatal(err)
	}
	owner := &securityUploadBody{Reader: strings.NewReader(content)}
	return &HTTPRequest{
		RawReq:    &http.Request{Method: http.MethodPost, Body: owner, Header: http.Header{}, ContentLength: declared},
		RawWriter: httptest.NewRecorder(),
		web:       &serverconfigs.HTTPWebConfig{IsOn: true, RequestLimit: limit},
	}, owner
}

func TestSecurityRequestLimitCoversUnknownLengthBeforeWAF(t *testing.T) {
	req, owner := securityBodyRequest(t, "123456789", -1)
	if err := req.prepareRequestBodyLimit(); err != nil {
		t.Fatal(err)
	}
	_, err := req.WAFReadBody(32)
	if !requests.IsBodyLimitError(err) {
		t.Fatalf("unknown-length inspection limit error = %v", err)
	}
	_ = req.RawReq.Body.Close()
	if owner.closed != 1 {
		t.Fatalf("original body close count = %d", owner.closed)
	}

	req, owner = securityBodyRequest(t, "123456789", -1)
	if err := req.prepareRequestBodyLimit(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(req.RawReq.Body)
	if !requests.IsBodyLimitError(err) || len(data) != 8 {
		t.Fatalf("streaming limit = %q, %v", data, err)
	}
	_ = req.RawReq.Body.Close()
	if owner.closed != 1 {
		t.Fatalf("stream body close count = %d", owner.closed)
	}
}

func TestSecurityRequestLimitKnownSizeAndReplayBoundary(t *testing.T) {
	req, _ := securityBodyRequest(t, "123456789", 9)
	if err := req.prepareRequestBodyLimit(); !requests.IsBodyLimitError(err) {
		t.Fatalf("declared size error = %v", err)
	}

	req, owner := securityBodyRequest(t, "12345678", -1)
	if err := req.prepareRequestBodyLimit(); err != nil {
		t.Fatal(err)
	}
	data, err := req.WAFReadBody(32)
	if err != nil || string(data) != "12345678" {
		t.Fatalf("exact body = %q, %v", data, err)
	}
	req.WAFRestoreBody(data)
	if err := req.prepareRequestBodyLimit(); err != nil {
		t.Fatal(err)
	}
	forwarded, err := io.ReadAll(req.RawReq.Body)
	if err != nil || string(forwarded) != "12345678" {
		t.Fatalf("replayed entity = %q, %v", forwarded, err)
	}
	_ = req.RawReq.Body.Close()
	if owner.closed != 1 {
		t.Fatalf("replayed original close count = %d", owner.closed)
	}
}

func TestSecurityRequestDoRejectsOversizeBeforeSecurityHandlers(t *testing.T) {
	limited, owner := securityBodyRequest(t, "123456789", 9)
	limited.RawReq.URL = &url.URL{Scheme: "http", Host: "example.test", Path: "/"}
	limited.RawReq.RemoteAddr = "198.51.100.8:1234"
	limited.ReqServer = &serverconfigs.ServerConfig{Web: limited.web}
	limited.disableLog = true
	limited.Do()
	if got := limited.RawWriter.(*httptest.ResponseRecorder).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("Do returned %d, want 413 before any downstream handler", got)
	}
	_ = limited.RawReq.Body.Close()
	if owner.closed != 1 {
		t.Fatalf("original body close count = %d", owner.closed)
	}
}
