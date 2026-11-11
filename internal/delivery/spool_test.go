package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDurableSpoolSurvivesRestartAndAck(t *testing.T) {
	dir := t.TempDir()
	queue := NewSpool(dir, 16, 8)
	id, err := queue.Append([]byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, id+".batch"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("batch permissions: %v %v", info, err)
	}
	restarted := NewSpool(dir, 16, 8)
	batches, err := restarted.Pending()
	if err != nil || len(batches) != 1 || string(batches[0].Data) != "request" || batches[0].ID != id {
		t.Fatalf("pending: %v %v", batches, err)
	}
	// A failed sender does not ack, so the exact identity survives retries.
	again, _ := restarted.Pending()
	if again[0].ID != id {
		t.Fatal("retry changed identity")
	}
	if err := restarted.Ack(id); err != nil {
		t.Fatal(err)
	}
	if batches, err := queue.Pending(); err != nil || len(batches) != 0 {
		t.Fatalf("ack: %v %v", batches, err)
	}
}

func TestDurableSpoolBoundsAndRejectsTraversal(t *testing.T) {
	queue := NewSpool(t.TempDir(), 8, 8)
	if _, err := queue.Append([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Append([]byte("1")); !errors.Is(err, ErrFull) {
		t.Fatalf("limit: %v", err)
	}
	if err := queue.Ack("../outside"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestAppendPreservesIdentityAfterPublishedSyncFailure(t *testing.T) {
	queue := NewSpool(t.TempDir(), 100, 100)
	queue.write = func(name string, data []byte) (bool, error) {
		if err := os.WriteFile(name, data, 0600); err != nil {
			return false, err
		}
		return true, errors.New("injected directory sync failure")
	}
	id, err := queue.Append([]byte("published"))
	if err == nil || id == "" {
		t.Fatal("ambiguous publication lost its batch identity")
	}
	pending, pendingErr := queue.Pending()
	if pendingErr != nil || len(pending) != 1 || pending[0].ID != id {
		t.Fatal("published batch cannot retry under its original identity", pendingErr)
	}
}

func TestAckConfirmsUnlinkDurability(t *testing.T) {
	queue := NewSpool(t.TempDir(), 100, 100)
	id, err := queue.Append([]byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	queue.syncDir = func(dir string) error { syncCalls++; return errors.New("injected ack sync failure") }
	if err := queue.Ack(id); err == nil {
		t.Fatal("ack reported success without durable unlink")
	}
	if syncCalls != 1 {
		t.Fatal("directory was not synced after ack")
	}
	queue.syncDir = func(string) error { syncCalls++; return nil }
	if err := queue.Ack(id); err != nil {
		t.Fatal("idempotent ack could not confirm prior deletion", err)
	}
	if syncCalls != 2 {
		t.Fatal("idempotent ack did not persist the previous unlink")
	}
}

func TestFullSpoolCanDrainAndAcceptNewBatches(t *testing.T) {
	queue := NewSpool(t.TempDir(), 8, 8)
	id, err := queue.Append([]byte("12345678"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Append([]byte("next")); !errors.Is(err, ErrFull) {
		t.Fatal("spool full was not detected")
	}
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatal("full spool cannot be read", err)
	}
	if err := queue.Ack(id); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Append([]byte("next")); err != nil {
		t.Fatal("draining did not release capacity", err)
	}
}

func TestCorruptSpoolEntryIsQuarantinedWithoutBlockingValidData(t *testing.T) {
	queue := NewSpool(t.TempDir(), 1<<20, 100)
	id, err := queue.Append([]byte("valid"))
	if err != nil {
		t.Fatal(err)
	}
	badID := "00000000000000000000000000000000"
	if err := os.WriteFile(filepath.Join(queue.dir, badID+".batch"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 || pending[0].ID != id {
		t.Fatal("corrupt head blocked valid batch", err)
	}
	if queue.Quarantined() != 1 {
		t.Fatal("quarantined file was not counted")
	}
	if _, err := os.Stat(filepath.Join(queue.dir, "quarantine", badID+".reason.json")); err != nil {
		t.Fatal("quarantine diagnosis missing", err)
	}
}

func TestQuarantineBudgetRetainsUnmovedBatch(t *testing.T) {
	queue := NewSpool(t.TempDir(), 1<<20, 100)
	id, err := queue.Append([]byte("retain-me"))
	if err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(queue.dir, "quarantine")
	if err := os.Mkdir(dead, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if err := os.WriteFile(filepath.Join(dead, fmt.Sprintf("%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.Quarantine(id, "permanent failure"); err == nil {
		t.Fatal("quarantine cap was not enforced")
	}
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 || string(pending[0].Data) != "retain-me" {
		t.Fatal("full quarantine discarded original data", err)
	}
}
