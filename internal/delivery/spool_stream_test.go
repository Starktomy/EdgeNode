package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeRestoredBatch(t *testing.T, dir string, number int, data []byte, modified time.Time) string {
	t.Helper()
	id := fmt.Sprintf("%032x", number)
	path := filepath.Join(dir, id+".batch")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSpoolOldestWindowAcrossDirectoryPages(t *testing.T) {
	queue := NewSpool(t.TempDir(), 1<<20, 1024)
	base := time.Unix(1700000000, 0)
	for i := 256; i >= 0; i-- {
		writeRestoredBatch(t, queue.dir, i, []byte("batch"), base.Add(time.Duration(i/3)*time.Second))
	}
	var want []string
	for i := 0; i < 8; i++ {
		want = append(want, fmt.Sprintf("%032x", i))
	}
	pending, err := queue.Pending()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, batch := range pending {
		got = append(got, batch.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("oldest batches or timestamp ties: got %v, want %v", got, want)
	}
	got = nil
	if err := queue.WalkPending(func(batch Batch) error {
		got = append(got, batch.ID)
		return nil
	}); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("streaming window: got %v, want %v, error %v", got, want, err)
	}
}

func TestSpoolCapacityObservesExternalRestoreAndDeletion(t *testing.T) {
	queue := NewSpool(t.TempDir(), 4, 4)
	if _, err := queue.Append([]byte("aa")); err != nil {
		t.Fatal(err)
	}
	restored := writeRestoredBatch(t, queue.dir, 1, []byte("bb"), time.Now())
	if _, err := queue.Append([]byte("c")); !errors.Is(err, ErrFull) {
		t.Fatalf("restored data bypassed disk budget: %v", err)
	}
	if err := os.Remove(filepath.Join(queue.dir, restored+".batch")); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Append([]byte("cc")); err != nil {
		t.Fatalf("externally released capacity remained stale: %v", err)
	}
}

func TestWalkPendingReadsLazilyWithoutHoldingProducerLock(t *testing.T) {
	queue := NewSpool(t.TempDir(), 1<<20, 1024)
	base := time.Unix(1700000000, 0)
	first := writeRestoredBatch(t, queue.dir, 1, []byte("first"), base)
	second := writeRestoredBatch(t, queue.dir, 2, []byte("before"), base.Add(time.Second))
	var got []string
	err := queue.WalkPending(func(batch Batch) error {
		got = append(got, batch.ID)
		if batch.ID == first {
			if err := os.WriteFile(filepath.Join(queue.dir, second+".batch"), []byte("latest"), 0600); err != nil {
				return err
			}
			// A sender blocked in a network callback must not block the producer
			// or require that callback to relinquish the current batch first.
			done := make(chan error, 1)
			go func() {
				_, err := queue.Append([]byte("new"))
				done <- errors.Join(err, queue.Ack(first))
			}()
			select {
			case err := <-done:
				return err
			case <-time.After(5 * time.Second):
				return errors.New("walk callback held the producer lock")
			}
		}
		if batch.ID != second || string(batch.Data) != "latest" {
			t.Fatalf("unvisited payload was eagerly retained or new batch escaped the selected window: %+v", batch)
		}
		return queue.Ack(batch.ID)
	})
	if err != nil || !reflect.DeepEqual(got, []string{first, second}) {
		t.Fatalf("streamed batches: %v, error %v", got, err)
	}
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 || string(pending[0].Data) != "new" {
		t.Fatal("concurrent producer batch was lost", err)
	}
}

func TestWalkPendingFailureKeepsOriginalAndUnvisitedIdentities(t *testing.T) {
	queue := NewSpool(t.TempDir(), 1<<20, 1024)
	base := time.Unix(1700000000, 0)
	first := writeRestoredBatch(t, queue.dir, 1, []byte("first"), base)
	second := writeRestoredBatch(t, queue.dir, 2, []byte("second"), base.Add(time.Second))
	responseLost := errors.New("response lost after commit")
	var visits int
	if err := queue.WalkPending(func(batch Batch) error {
		visits++
		if batch.ID != first {
			t.Fatalf("retry identity changed: %s", batch.ID)
		}
		return responseLost
	}); !errors.Is(err, responseLost) || visits != 1 {
		t.Fatalf("failed callback did not stop the walk: %d visits, %v", visits, err)
	}
	var retried []string
	if err := queue.WalkPending(func(batch Batch) error {
		retried = append(retried, batch.ID)
		return queue.Ack(batch.ID)
	}); err != nil || !reflect.DeepEqual(retried, []string{first, second}) {
		t.Fatalf("unacknowledged disk identities changed: %v, %v", retried, err)
	}
}

func TestWalkPendingRevalidatesChangedSelectedFiles(t *testing.T) {
	for _, replacement := range []string{"deleted", "empty", "oversize", "symlink", "directory"} {
		t.Run(replacement, func(t *testing.T) {
			queue := NewSpool(t.TempDir(), 1<<20, 8)
			base := time.Unix(1700000000, 0)
			first := writeRestoredBatch(t, queue.dir, 1, []byte("first"), base)
			changed := writeRestoredBatch(t, queue.dir, 2, []byte("before"), base.Add(time.Second))
			third := writeRestoredBatch(t, queue.dir, 3, []byte("third"), base.Add(2*time.Second))
			outside := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			var got []string
			err := queue.WalkPending(func(batch Batch) error {
				got = append(got, batch.ID)
				if batch.ID != first {
					return queue.Ack(batch.ID)
				}
				path := filepath.Join(queue.dir, changed+".batch")
				if err := os.Remove(path); err != nil {
					return err
				}
				switch replacement {
				case "empty":
					return os.WriteFile(path, nil, 0600)
				case "oversize":
					return os.WriteFile(path, []byte("oversized"), 0600)
				case "symlink":
					return os.Symlink(outside, path)
				case "directory":
					return os.Mkdir(path, 0700)
				default:
					return nil
				}
			})
			if err != nil || !reflect.DeepEqual(got, []string{first, third}) {
				t.Fatalf("changed file blocked valid delivery or was read: %v, %v", got, err)
			}
			wantQuarantined := uint64(1)
			if replacement == "deleted" {
				wantQuarantined = 0
			}
			if queue.Quarantined() != wantQuarantined {
				t.Fatalf("invalid changed file was not isolated: %d", queue.Quarantined())
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "secret" {
				t.Fatal("spool followed or modified a symlink target", err)
			}
		})
	}
}

func TestPendingAggregatePayloadBudgetPreservesUnvisitedBatches(t *testing.T) {
	queue := NewSpool(t.TempDir(), 100, 6)
	base := time.Unix(1700000000, 0)
	var want []string
	for i := 0; i < 3; i++ {
		want = append(want, writeRestoredBatch(t, queue.dir, i, []byte("four"), base.Add(time.Duration(i)*time.Second)))
	}
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 || pending[0].ID != want[0] {
		t.Fatalf("pending exceeded its aggregate budget: %d batches, %v", len(pending), err)
	}
	var got []string
	if err := queue.WalkPending(func(batch Batch) error {
		got = append(got, batch.ID)
		return nil
	}); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("budgeting altered disk identities: %v, %v", got, err)
	}
}

func TestWalkPendingFullQuarantineRetainsChangedAndUnvisitedFiles(t *testing.T) {
	queue := NewSpool(t.TempDir(), 1<<20, 1024)
	base := time.Unix(1700000000, 0)
	first := writeRestoredBatch(t, queue.dir, 1, []byte("first"), base)
	changed := writeRestoredBatch(t, queue.dir, 2, []byte("before"), base.Add(time.Second))
	third := writeRestoredBatch(t, queue.dir, 3, []byte("third"), base.Add(2*time.Second))
	dead := filepath.Join(queue.dir, "quarantine")
	if err := os.Mkdir(dead, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if err := os.WriteFile(filepath.Join(dead, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var visits int
	err := queue.WalkPending(func(batch Batch) error {
		visits++
		if batch.ID != first {
			t.Fatal("quarantine failure advanced past the changed batch")
		}
		return os.WriteFile(filepath.Join(queue.dir, changed+".batch"), nil, 0600)
	})
	if err == nil || visits != 1 {
		t.Fatalf("full quarantine was hidden: %d visits, %v", visits, err)
	}
	if data, err := os.ReadFile(filepath.Join(queue.dir, changed+".batch")); err != nil || len(data) != 0 {
		t.Fatal("changed batch was dropped after a quarantine failure", err)
	}
	if data, err := os.ReadFile(filepath.Join(queue.dir, third+".batch")); err != nil || string(data) != "third" {
		t.Fatal("unvisited batch identity or payload changed", err)
	}
}
