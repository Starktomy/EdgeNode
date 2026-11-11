// Package httpcache implements HTTP cache decisions independently of storage.
package httpcache

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VaryMetadataHeader is stored inside cache entries and never sent to clients.
const VaryMetadataHeader = "X-Edge-Cache-Vary-Metadata"

type Control map[string][]string

// ParseControl accepts multiple field lines and commas inside quoted values.
func ParseControl(header http.Header) Control {
	control := Control{}
	for _, line := range header.Values("Cache-Control") {
		for _, directive := range splitList(line) {
			name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				control[name] = append(control[name], strings.Trim(strings.TrimSpace(value), "\""))
			}
		}
	}
	return control
}

func (c Control) Has(name string) bool {
	_, found := c[name]
	return found
}

// Seconds treats malformed and duplicate freshness directives as stale.
func (c Control) Seconds(name string) (seconds int64, present, valid bool) {
	values, found := c[name]
	if !found {
		return 0, false, false
	}
	if len(values) != 1 || values[0] == "" {
		return 0, true, false
	}
	for _, digit := range values[0] {
		if digit < '0' || digit > '9' {
			return 0, true, false
		}
	}
	seconds, err := strconv.ParseInt(values[0], 10, 64)
	return seconds, true, err == nil
}

// SharedResponseAllowed excludes responses that require validation, which the
// current storage path cannot perform. Explicitly public authenticated responses
// can be shared; other authenticated responses must go back to the origin.
func SharedResponseAllowed(requestHeader, responseHeader http.Header) bool {
	c := ParseControl(responseHeader)
	if c.Has("no-store") || c.Has("private") || c.Has("no-cache") {
		return false
	}
	publicValues := c["public"]
	explicitPublic := len(publicValues) == 1 && publicValues[0] == ""
	sharedSeconds, sharedPresent, sharedValid := c.Seconds("s-maxage")
	explicitShared := sharedPresent && sharedValid && sharedSeconds > 0
	if (requestHeader.Get("Authorization") != "" || requestHeader.Get("Cookie") != "" || responseHeader.Get("Set-Cookie") != "") && !explicitPublic && !explicitShared {
		return false
	}
	return true
}

// Lifetime gives shared-cache freshness priority to s-maxage and accounts for
// time already spent in an upstream cache. A zero lifetime must not be stored.
func Lifetime(header http.Header, fallback int64) (int64, bool) {
	c := ParseControl(header)
	life := fallback
	for _, name := range []string{"s-maxage", "max-age"} {
		seconds, present, valid := c.Seconds(name)
		if present {
			if !valid {
				return 0, false
			}
			life = seconds
			break
		}
	}
	if age, err := strconv.ParseInt(strings.TrimSpace(header.Get("Age")), 10, 64); err == nil && age > 0 {
		life -= age
	}
	return life, life > 0
}

func VaryFields(header http.Header) ([]string, bool) {
	fields := map[string]bool{}
	for _, line := range header.Values("Vary") {
		for _, field := range strings.Split(line, ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			if field == "*" || !validFieldName(field) {
				return nil, false
			}
			fields[http.CanonicalHeaderKey(field)] = true
		}
	}
	if len(fields) > 32 {
		return nil, false
	}
	result := make([]string, 0, len(fields))
	for field := range fields {
		result = append(result, field)
	}
	sort.Strings(result)
	return result, true
}

// EncodeVary binds a stored representation to the original request. Keeping
// metadata in the entry also preserves safety after a restart or configuration
// change. Storage may retain one representation per configured cache key.
func EncodeVary(responseHeader, requestHeader http.Header) (string, bool) {
	fields, ok := VaryFields(responseHeader)
	if !ok {
		return "", false
	}
	if len(fields) == 0 {
		return "", true
	}
	snapshot := make(map[string][]string, len(fields))
	for _, field := range fields {
		snapshot[field] = requestValues(requestHeader, field)
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > 16<<10 {
		return "", false
	}
	return base64.RawStdEncoding.EncodeToString(data), true
}

func MatchesVary(responseHeader, requestHeader http.Header) bool {
	fields, ok := VaryFields(responseHeader)
	if !ok {
		return false
	}
	if len(fields) == 0 {
		return true
	}
	encoded := responseHeader.Get(VaryMetadataHeader)
	if len(encoded) > 24<<10 {
		return false
	}
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	var snapshot map[string][]string
	if json.Unmarshal(data, &snapshot) != nil || len(snapshot) != len(fields) {
		return false
	}
	for _, field := range fields {
		values, found := snapshot[field]
		current, present := requestHeader[field]
		if !found || (values == nil) != !present {
			return false
		}
		// The common case uses identical field-line boundaries. Trim while
		// comparing instead of allocating another slice and joined strings.
		if equalRequestValues(values, current) {
			continue
		}
		// Retain compatibility when a proxy combines field lines, including
		// commas already present in an individual value.
		if strings.Join(values, ",") != strings.Join(requestValues(requestHeader, field), ",") {
			return false
		}
	}
	return true
}

func equalRequestValues(stored, current []string) bool {
	if len(stored) != len(current) {
		return false
	}
	for index, value := range current {
		if stored[index] != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

func MergeVary(header http.Header, field string) {
	fields, ok := VaryFields(header)
	if !ok {
		header.Set("Vary", "*")
		return
	}
	canonical := http.CanonicalHeaderKey(field)
	for _, existing := range fields {
		if existing == canonical {
			return
		}
	}
	fields = append(fields, canonical)
	header.Set("Vary", strings.Join(fields, ", "))
}

// NotModified implements GET/HEAD conditional precedence and weak ETag matching.
func NotModified(request *http.Request, etag, lastModified string) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return false
	}
	if values, present := request.Header["If-None-Match"]; present {
		for _, candidate := range splitList(strings.Join(values, ",")) {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || (etag != "" && strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(etag, "W/")) {
				return true
			}
		}
		return false
	}
	modified, err := http.ParseTime(lastModified)
	if err != nil {
		return false
	}
	since, err := http.ParseTime(request.Header.Get("If-Modified-Since"))
	return err == nil && !modified.Truncate(time.Second).After(since)
}

func requestValues(header http.Header, field string) []string {
	values, present := header[field]
	if !present {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = strings.TrimSpace(value)
	}
	return result
}

func validFieldName(name string) bool {
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return name != ""
}

func splitList(value string) []string {
	var result []string
	start := 0
	quoted, escaped := false, false
	for i, c := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
		} else if c == ',' && !quoted {
			result = append(result, value[start:i])
			start = i + 1
		}
	}
	return append(result, value[start:])
}
