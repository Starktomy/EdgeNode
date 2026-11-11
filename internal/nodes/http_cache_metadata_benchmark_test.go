package nodes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/shared"
	"github.com/TeaOSLab/EdgeNode/internal/caches"
	"github.com/TeaOSLab/EdgeNode/internal/httpcache"
)

type metadataBenchmarkWriter struct{ header http.Header }

func (w metadataBenchmarkWriter) Header() http.Header         { return w.header }
func (w metadataBenchmarkWriter) WriteHeader(int)             {}
func (w metadataBenchmarkWriter) Write(p []byte) (int, error) { return len(p), nil }

type metadataDiscardWriter struct{}

func (metadataDiscardWriter) WriteString(value string) (int, error) { return len(value), nil }

var benchmarkCacheHeaderLength int

// Exercise one fresh HTTPWriter per response. Reusing it across iterations
// would incorrectly measure memoization surviving between unrelated requests.
func BenchmarkCacheResponseMetadata(b *testing.B) {
	previous := caches.SharedManager
	caches.SharedManager = caches.NewManager()
	defer func() {
		caches.SharedManager.UpdatePolicies(nil)
		caches.SharedManager = previous
	}()
	policy := &serverconfigs.HTTPCachePolicy{Id: 91472, IsOn: true, Type: serverconfigs.CachePolicyStorageMemory, Capacity: shared.NewSizeCapacity(1, shared.SizeCapacityUnitMB)}
	caches.SharedManager.UpdatePolicies([]*serverconfigs.HTTPCachePolicy{policy})
	ref := &serverconfigs.HTTPCacheRef{IsOn: true, Key: "metadata-benchmark"}
	if err := ref.Init(); err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		vary        []string
		compression bool
	}{
		{"NoVary", nil, false},
		{"OriginAndLanguage", []string{"Origin", "Accept-Language"}, false},
		{"OriginPlusCompression", []string{"Origin", "Accept-Language"}, true},
	} {
		b.Run(test.name, func(b *testing.B) {
			raw := httptest.NewRequest(http.MethodGet, "https://cache.example/data", nil)
			raw.Header = http.Header{"Origin": {"https://tenant.example"}, "Accept-Language": {"en-US", "en;q=0.9"}, "Accept-Encoding": {"gzip, br"}}
			request := &HTTPRequest{RawReq: raw, cacheRequestHeader: raw.Header.Clone(), cacheRef: ref, cacheKey: ref.Key, ReqServer: &serverconfigs.ServerConfig{Id: 1, HTTPCachePolicy: policy}, web: &serverconfigs.HTTPWebConfig{Cache: &serverconfigs.HTTPCacheConfig{IsOn: true, EnableCacheControlMaxAge: true}}, varMapping: map[string]string{}}
			base := http.Header{"Cache-Control": {"public, max-age=60"}, "Content-Type": {"text/plain"}, "Content-Length": {"0"}}
			if test.vary != nil {
				base["Vary"] = test.vary
			}
			response := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}
			b.ReportAllocs()
			for b.Loop() {
				writer := &HTTPWriter{req: request, rawWriter: metadataBenchmarkWriter{base.Clone()}, statusCode: http.StatusOK}
				writer.PrepareCache(response, 0)
				if writer.cacheWriter == nil {
					b.Fatal("fixture response did not open cache storage")
				}
				if test.compression {
					// Compression changes Vary before its own size and header writes.
					httpcache.MergeVary(writer.Header(), "Accept-Encoding")
					benchmarkCacheHeaderLength = writer.calculateHeaderLength()
					writer.appendVaryMetadata(metadataDiscardWriter{})
				}
				if err := writer.cacheWriter.Discard(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
