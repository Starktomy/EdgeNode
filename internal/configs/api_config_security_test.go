package configs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityNodeAPIConfigUsesPrivateAtomicFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "api_node.yaml")
	if err := os.WriteFile(filename, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	config := &APIConfig{NodeId: "fixture-node", Secret: "private-fixture", RPCEndpoints: []string{"https://api.example.test:8443"}}
	if err := config.WriteFile(filename); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filename)
	dirInfo, _ := os.Stat(dir)
	if info.Mode().Perm() != 0600 || dirInfo.Mode().Perm() != 0700 {
		t.Fatal("configuration credentials are exposed")
	}
}
