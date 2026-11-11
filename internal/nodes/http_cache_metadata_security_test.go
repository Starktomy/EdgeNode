package nodes

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/httpcache"
)

func metadataTestWriter(requestHeader, responseHeader http.Header) *HTTPWriter {
	raw := httptest.NewRequest(http.MethodGet, "https://cache.example/data", nil)
	raw.Header = requestHeader.Clone()
	request := &HTTPRequest{RawReq: raw, cacheRequestHeader: raw.Header.Clone()}
	recorder := httptest.NewRecorder()
	for name, values := range responseHeader {
		recorder.Header()[name] = append([]string(nil), values...)
	}
	return NewHTTPWriter(request, recorder)
}

func storedMetadataHeader(t *testing.T, writer *HTTPWriter) http.Header {
	t.Helper()
	var encoded bytes.Buffer
	writer.appendVaryMetadata(&encoded)
	line, ok := writer.cacheVaryMetadata()
	if !ok || encoded.String() != line {
		t.Fatal("cache header sizing and serialization disagree")
	}
	header := writer.Header().Clone()
	if line != "" {
		value := strings.TrimSuffix(strings.TrimPrefix(line, httpcache.VaryMetadataHeader+":"), "\n")
		header.Set(httpcache.VaryMetadataHeader, value)
	}
	return header
}

func TestSecurityCacheMetadataTracksCompressionAndInPlaceVaryChanges(t *testing.T) {
	original := http.Header{"Origin": {"https://tenant-a.example"}, "Accept-Encoding": {"gzip"}, "Accept-Language": {"zh-CN"}}
	writer := metadataTestWriter(original, http.Header{"Vary": {"Origin"}, "Content-Type": {"text/plain"}})
	first := storedMetadataHeader(t, writer)
	if !httpcache.MatchesVary(first, original) {
		t.Fatal("original representation rejected")
	}
	other := original.Clone()
	other.Set("Origin", "https://tenant-b.example")
	if httpcache.MatchesVary(first, other) {
		t.Fatal("cached metadata lost origin isolation")
	}

	// Origin writers have already serialized the first representation. The
	// compression writer adds a different Vary requirement to the same response.
	httpcache.MergeVary(writer.Header(), "Accept-Encoding")
	compressed := storedMetadataHeader(t, writer)
	other = original.Clone()
	other.Set("Accept-Encoding", "br")
	if !httpcache.MatchesVary(compressed, original) || httpcache.MatchesVary(compressed, other) {
		t.Fatal("compression reused metadata without the new Vary field")
	}

	// A caller may edit an existing field-line slice without replacing the
	// header map entry. Memoization must own its old field-line values.
	writer.Header()["Vary"][0] = "Accept-Language"
	changed := storedMetadataHeader(t, writer)
	other = original.Clone()
	other.Set("Accept-Language", "en-US")
	if !httpcache.MatchesVary(changed, original) || httpcache.MatchesVary(changed, other) {
		t.Fatal("in-place Vary edit reused the previous representation")
	}

	writer.Header().Set("Vary", "*")
	if line, valid := writer.cacheVaryMetadata(); valid || line != "" {
		t.Fatal("Vary:* retained previously valid cache metadata")
	}
	writer.Header().Del("Vary")
	if line, valid := writer.cacheVaryMetadata(); !valid || line != "" {
		t.Fatal("removing Vary retained stale metadata")
	}
}

func TestSecurityCacheMetadataRemainsBoundToOriginalRequestSnapshot(t *testing.T) {
	original := http.Header{"Origin": {"https://tenant-a.example"}}
	writer := metadataTestWriter(original, http.Header{"Vary": {"Origin"}})
	first := storedMetadataHeader(t, writer)
	writer.req.RawReq.Header.Set("Origin", "https://rewritten-origin.example")
	after := storedMetadataHeader(t, writer)
	if first.Get(httpcache.VaryMetadataHeader) != after.Get(httpcache.VaryMetadataHeader) ||
		!httpcache.MatchesVary(after, original) || httpcache.MatchesVary(after, writer.req.RawReq.Header) {
		t.Fatal("proxy header changes replaced the original client representation")
	}
	second := metadataTestWriter(writer.req.RawReq.Header, writer.Header())
	other := storedMetadataHeader(t, second)
	if other.Get(httpcache.VaryMetadataHeader) == after.Get(httpcache.VaryMetadataHeader) {
		t.Fatal("different responses shared serialized request metadata")
	}
}

func TestSecurityCacheMetadataConcurrentResponsesAndLength(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			origin := "https://tenant-" + strings.Repeat("a", index+1) + ".example"
			request := http.Header{"Origin": {origin}}
			writer := metadataTestWriter(request, http.Header{"Vary": {"Origin"}, "Content-Type": {"text/plain"}})
			for j := 0; j < 10; j++ {
				header := storedMetadataHeader(t, writer)
				if !httpcache.MatchesVary(header, request) {
					t.Error("concurrent responses mixed representation metadata")
				}
				metadata, _ := writer.cacheVaryMetadata()
				plainSize := len("Vary:Origin\nContent-Type:text/plain\n")
				if writer.calculateHeaderLength() != plainSize+len(metadata) {
					t.Error("cache entry header allocation has the wrong size")
				}
			}
		}(i)
	}
	wg.Wait()
}
