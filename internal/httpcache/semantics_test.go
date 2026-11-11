package httpcache

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSharedFreshness(t *testing.T) {
	for _, tc := range []struct {
		control string
		age     string
		want    int64
		valid   bool
	}{
		{"public, max-age=300", "", 300, true},
		{"public, max-age=300, s-maxage=90", "10", 80, true},
		{"max-age=\"60\", extension=\"a,b\"", "", 60, true},
		{"max-age=0", "", 0, false},
		{"max-age=-1", "", 0, false},
		{"max-age=5, max-age=10", "", 0, false},
		{"max-age=60", "60", 0, false},
		{"s-maxage=invalid, max-age=60", "", 0, false},
	} {
		t.Run(tc.control+tc.age, func(t *testing.T) {
			h := http.Header{"Cache-Control": {tc.control}, "Age": {tc.age}}
			got, valid := Lifetime(h, 600)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("freshness = (%d,%v), want (%d,%v)", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestSensitiveResponsesCannotBeShared(t *testing.T) {
	request := http.Header{"Authorization": {"Bearer private"}}
	for _, control := range []string{"", "private", "public, no-store", "no-cache=\"Set-Cookie\"", "s-maxage=invalid", "s-maxage=0", "s-maxage=60, s-maxage=90", "public=invalid"} {
		if SharedResponseAllowed(request, http.Header{"Cache-Control": {control}}) {
			t.Errorf("allowed authenticated response with %q", control)
		}
	}
	if !SharedResponseAllowed(request, http.Header{"Cache-Control": {"public, max-age=60"}}) {
		t.Fatal("explicitly public response was rejected")
	}
	if SharedResponseAllowed(http.Header{"Cookie": {"sid=private"}}, http.Header{"Cache-Control": {"max-age=60"}}) {
		t.Fatal("session-dependent response without explicit shared permission was cached")
	}
	if SharedResponseAllowed(nil, http.Header{"Set-Cookie": {"sid=private"}}) {
		t.Fatal("session-creating response without explicit shared permission was cached")
	}
}

func TestVarySurvivesRestartAndRejectsOtherRepresentations(t *testing.T) {
	response := http.Header{"Vary": {"Accept-Language, Origin"}}
	request := http.Header{"Accept-Language": {"zh-CN"}, "Origin": {"https://a.example"}}
	metadata, ok := EncodeVary(response, request)
	if !ok {
		t.Fatal("could not encode representation")
	}
	response.Set(VaryMetadataHeader, metadata)
	if !MatchesVary(response.Clone(), request.Clone()) {
		t.Fatal("stored representation did not match original request")
	}
	request.Set("Origin", "https://b.example")
	if MatchesVary(response, request) {
		t.Fatal("representation leaked across origins")
	}
	response.Del(VaryMetadataHeader)
	if MatchesVary(response, request) {
		t.Fatal("legacy entry with unrecorded Vary must be a miss")
	}
	response.Set("Vary", "*")
	if _, ok := EncodeVary(response, request); ok {
		t.Fatal("Vary:* must not be stored")
	}
}

func TestCompressionPreservesOriginVary(t *testing.T) {
	h := http.Header{"Vary": {"Origin", "Accept-Language"}}
	MergeVary(h, "Accept-Encoding")
	MergeVary(h, "accept-encoding")
	fields, ok := VaryFields(h)
	if !ok || len(fields) != 3 {
		t.Fatalf("Vary fields = %v, %v", fields, ok)
	}
}

func TestConditionalRequestsRespectETagPrecedence(t *testing.T) {
	const modified = "Mon, 02 Jan 2006 15:04:05 GMT"
	request := httptest.NewRequest(http.MethodGet, "https://example.test/a", nil)
	request.Header.Set("If-Modified-Since", modified)
	request.Header.Set("If-None-Match", `"old"`)
	if NotModified(request, `"new"`, modified) {
		t.Fatal("matching date must not override a mismatching ETag")
	}
	request.Header.Set("If-None-Match", `"old", W/"new"`)
	if !NotModified(request, `"new"`, modified) {
		t.Fatal("weak ETag in a list should match")
	}
	request.Header.Del("If-None-Match")
	request.Header.Set("If-Modified-Since", "Mon, 02 Jan 2006 15:05:05 GMT")
	if !NotModified(request, `"new"`, modified) {
		t.Fatal("a representation older than the validator should match")
	}
	request.Method = http.MethodPost
	if NotModified(request, `"new"`, modified) {
		t.Fatal("POST must not become a 304 response")
	}
}
