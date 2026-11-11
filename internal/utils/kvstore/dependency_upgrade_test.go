package kvstore_test

import (
	"bytes"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/utils/kvstore"
)

func TestDependencyUpgradePebbleReopensExistingStore(t *testing.T) {
	dir := t.TempDir()
	store, err := kvstore.OpenStoreDir(dir, "compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Set([]byte("sample"), []byte("persisted-value")); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = kvstore.OpenStoreDir(dir, "compatibility")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value, closer, err := store.Get([]byte("sample"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(value, []byte("persisted-value")) {
		t.Fatalf("reopened value = %q", value)
	}
	if err = closer.Close(); err != nil {
		t.Fatal(err)
	}
}
