package nodes

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityRootPathRejectsTraversalAfterDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, raw      string
		decode, hidden bool
	}{
		{"parent", "/../secret", false, false},
		{"encoded", "/%2e%2e/secret", false, false},
		{"double-encoded", "/%252e%252e/secret", true, true},
		{"backslash", "/dir%5c..%5csecret", false, false},
		{"hidden-after-decode", "/%252esecret", true, true},
		{"nul", "/file%00.txt", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.ParseRequestURI(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if name, err := normalizeRootPath(parsed.Path, tc.decode, "", tc.hidden); err == nil {
				t.Fatalf("accepted %q as %q", tc.raw, name)
			}
		})
	}
	name, err := normalizeRootPath("/prefix/a+b.txt", true, "/prefix", true)
	if err != nil || name != "a+b.txt" {
		t.Fatalf("literal plus changed: %q, %v", name, err)
	}
}

func TestSecurityRootOpenConfinesFilesIndexesAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "public")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "index.html"), []byte("safe index"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret", filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("../../secret", filepath.Join(root, "dir", "bad.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir/index.html", filepath.Join(root, "inside-link")); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"../secret", "outside-link", "dir/bad.html"} {
		file, _, err := openRootFile(root, relative, nil, false)
		if err == nil {
			_ = file.Close()
			t.Fatalf("opened root escape %q", relative)
		}
	}
	for _, relative := range []string{"dir", "inside-link"} {
		file, _, err := openRootFile(root, relative, []string{"bad*.html", "index*.html"}, true)
		if err != nil {
			t.Fatalf("open %q: %v", relative, err)
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || string(data) != "safe index" {
			t.Fatalf("read %q = %q, %v", relative, data, err)
		}
	}
	file, _, err := openRootFile(root, "dir", []string{"../../secret"}, false)
	if err == nil {
		_ = file.Close()
		t.Fatal("escaped through configured index")
	}
}
