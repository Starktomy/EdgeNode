package httpcache

import (
	"net/http"
	"testing"
)

var benchmarkVaryMatch bool

func BenchmarkMatchesVary(b *testing.B) {
	for _, test := range []struct {
		name    string
		vary    string
		request http.Header
	}{
		{"NoVary", "", http.Header{}},
		{"Origin", "Origin", http.Header{"Origin": {"https://tenant.example"}}},
		{"OriginAndLanguage", "Origin, Accept-Language", http.Header{"Origin": {"https://tenant.example"}, "Accept-Language": {" en-US, en;q=0.9 "}}},
		{"MultipleFieldLines", "Origin, Accept-Language", http.Header{"Origin": {"https://tenant.example"}, "Accept-Language": {" en-US ", " en;q=0.9 "}}},
	} {
		b.Run(test.name, func(b *testing.B) {
			response := http.Header{}
			if test.vary != "" {
				response.Set("Vary", test.vary)
			}
			encoded, ok := EncodeVary(response, test.request)
			if !ok {
				b.Fatal("fixture representation could not be encoded")
			}
			response.Set(VaryMetadataHeader, encoded)
			b.ReportAllocs()
			for b.Loop() {
				benchmarkVaryMatch = MatchesVary(response, test.request)
			}
			if !benchmarkVaryMatch {
				b.Fatal("matching representation was rejected")
			}
		})
	}
}
