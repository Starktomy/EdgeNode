package nodes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

func TestSecurityAuthPolicyFailureStopsBeforeProtectedRoot(t *testing.T) {
	rootDir := t.TempDir()
	const protected = "protected-content-must-never-be-sent"
	if err := os.WriteFile(filepath.Join(rootDir, "secret.txt"), []byte(protected), 0600); err != nil {
		t.Fatal(err)
	}
	unknown := &serverconfigs.HTTPAuthPolicy{IsOn: true, Type: "unknown-auth-method"}
	if err := unknown.Init(); err == nil {
		t.Fatal("unknown policy unexpectedly initialized")
	}
	for _, tc := range []struct {
		name   string
		ref    *serverconfigs.HTTPAuthPolicyRef
		status int
	}{
		{"failed-initialization", &serverconfigs.HTTPAuthPolicyRef{IsOn: true, AuthPolicy: unknown}, http.StatusInternalServerError},
		{"missing-enabled-policy", &serverconfigs.HTTPAuthPolicyRef{IsOn: true}, http.StatusServiceUnavailable},
		{"nil-reference", nil, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			web := &serverconfigs.HTTPWebConfig{
				IsOn: true,
				Auth: &serverconfigs.HTTPAuthConfig{IsOn: true, PolicyRefs: []*serverconfigs.HTTPAuthPolicyRef{tc.ref}},
				Root: &serverconfigs.HTTPRootConfig{IsOn: true, Dir: rootDir},
			}
			raw := httptest.NewRequest(http.MethodGet, "http://auth.example.test/secret.txt", nil)
			raw.RemoteAddr = "198.51.100.9:12345"
			recorder := httptest.NewRecorder()
			request := &HTTPRequest{
				RawReq: raw, RawWriter: recorder, ReqHost: "auth.example.test",
				ReqServer:  &serverconfigs.ServerConfig{Id: 7361, Web: web},
				disableLog: true, disableMetrics: true,
			}
			request.Do()
			if recorder.Code != tc.status || strings.Contains(recorder.Body.String(), protected) {
				t.Fatalf("failed authentication continued to protected handler: status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}
