package caches_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/TeaOSLab/EdgeNode/internal/caches"
)

func TestDependencyUpgradeSQLiteCacheIndexReopens(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cache-index.db")
	index := caches.NewSQLiteFileListDB()
	if err := index.Open(file); err != nil {
		t.Fatal(err)
	}
	if err := index.Init(); err != nil {
		t.Fatal(err)
	}
	item := &caches.Item{Key: "https://example.test/asset.css", BodySize: 64, HeaderSize: 32, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := index.AddSync("0123456789abcdef0123456789abcdef", item); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index = caches.NewSQLiteFileListDB()
	if err := index.Open(file); err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err := index.Init(); err != nil {
		t.Fatal(err)
	}
	if count, err := index.Total(); err != nil || count != 1 {
		t.Fatalf("reopened index count = %d, %v", count, err)
	}
	if err := index.DeleteSync("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if count, err := index.Total(); err != nil || count != 0 {
		t.Fatalf("deleted index count = %d, %v", count, err)
	}
}
