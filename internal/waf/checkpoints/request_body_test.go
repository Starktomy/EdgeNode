package checkpoints

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
)

func TestRequestBodyCheckpoint_RequestValue(t *testing.T) {
	rawReq, err := http.NewRequest(http.MethodPost, "http://teaos.cn", bytes.NewBuffer([]byte("123456")))
	if err != nil {
		t.Fatal(err)
	}
	var req = requests.NewTestRequest(rawReq)
	checkpoint := new(RequestBodyCheckpoint)
	t.Log(checkpoint.RequestValue(req, "", nil, 1))

	body, err := io.ReadAll(rawReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(body))
	t.Log(string(req.WAFGetCacheBody()))
}

func TestRequestBodyCheckpoint_RequestValue_Max(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.test", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	if err != nil {
		t.Fatal(err)
	}

	checkpoint := new(RequestBodyCheckpoint)
	value, _, err, _ := checkpoint.RequestValue(requests.NewTestRequest(req), "", nil, 1)
	if !requests.IsBodyLimitError(err) {
		t.Fatalf("oversized body inspected successfully: value=%v, err=%v", value, err)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != (1<<20)+1 {
		t.Fatalf("request entity changed after rejected inspection: %d bytes", len(body))
	}
	_ = req.Body.Close()
}
