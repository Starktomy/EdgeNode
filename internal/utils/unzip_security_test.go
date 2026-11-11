package utils

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func nodeArchiveFixture(t *testing.T, names ...string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "node.zip")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0755)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte("new executable"))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestSecurityNodeArchiveRejectsPathsAndLinksBeforeReplacement(t *testing.T) {
	for _, name := range []string{"edge-node/../outside", "/edge-node/absolute", "other/bin/file", "edge-node/bin\\file", "edge-node/bin/file:stream"} {
		if err := NewUnzip(nodeArchiveFixture(t, "edge-node/bin/edge-node", name), t.TempDir(), "edge-node/").Run(); err == nil {
			t.Fatalf("unsafe archive path accepted: %q", name)
		}
	}
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := NewUnzip(nodeArchiveFixture(t, "edge-node/bin/edge-node"), dir, "edge-node/").Run(); err == nil {
		t.Fatal("extraction followed an outside symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "edge-node")); !os.IsNotExist(err) {
		t.Fatal("outside archive file was created")
	}
}

func TestSecurityNodeArchiveReplacesExistingBinaryWithPrivateFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "bin", "edge-node")
	if err := os.WriteFile(filename, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := NewUnzip(nodeArchiveFixture(t, "edge-node/bin/edge-node"), dir, "edge-node/").Run(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filename)
	info, _ := os.Stat(filename)
	if string(data) != "new executable" || info.Mode().Perm() != 0700 {
		t.Fatal("binary replacement or private mode failed")
	}
	if err := NewUnzip(nodeArchiveFixture(t, "edge-node/bin/edge-node", "edge-node/bin/edge-node"), dir, "edge-node/").Run(); err == nil {
		t.Fatal("duplicate archive entries accepted")
	}
}
