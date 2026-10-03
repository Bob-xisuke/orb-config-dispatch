package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleRollout(entries []Entry) *Rollout {
	return &Rollout{
		ConfigName:        "payments",
		TargetEnvironment: "staging",
		Namespace:         "billing",
		Version:           1,
		Entries:           entries,
	}
}

func TestDigestIgnoresOrderAndTracksContent(t *testing.T) {
	one := []Entry{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}
	reordered := []Entry{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}}
	valueChanged := []Entry{{Key: "a", Value: "1"}, {Key: "b", Value: "3"}}
	keyChanged := []Entry{{Key: "a", Value: "1"}, {Key: "c", Value: "2"}}

	if Digest(one) != Digest(reordered) {
		t.Fatal("digest must not depend on entry order")
	}
	for name, other := range map[string][]Entry{"value": valueChanged, "key": keyChanged} {
		if Digest(one) == Digest(other) {
			t.Fatalf("digest must change when %s changes", name)
		}
	}
}

func TestDigestIsStableAcrossStore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rollout := sampleRollout([]Entry{{Key: "z", Value: "9"}, {Key: "a", Value: "1"}})
	wantDigest := Digest(rollout.Entries)

	stored, err := st.Register(ctx, rollout)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if stored.Digest != wantDigest || len(stored.Digest) != 64 {
		t.Fatalf("digest = %q, want %q (64 hex chars)", stored.Digest, wantDigest)
	}
	if stored.Revoked {
		t.Fatal("new rollout must not be revoked")
	}
	if stored.Entries[0].Key != "a" {
		t.Fatalf("entries must be sorted by key, got %q first", stored.Entries[0].Key)
	}

	reopened := mustReopen(t, st)
	listed, err := reopened.List(ctx, "staging", "billing", "payments")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Digest != wantDigest {
		t.Fatalf("digest %q must survive storage restart", wantDigest)
	}
}

func TestRegisterSameContentIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first, err := st.Register(ctx, sampleRollout([]Entry{{Key: "a", Value: "1"}}))
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	second, err := st.Register(ctx, sampleRollout([]Entry{{Key: "a", Value: "1"}}))
	if err != nil {
		t.Fatalf("duplicate register: %v", err)
	}
	if second.Digest != first.Digest {
		t.Fatal("duplicate submission must return the existing record")
	}

	listed, err := st.List(ctx, "staging", "billing", "payments")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("identical resubmission must not add a row, got %d", len(listed))
	}
}

func TestRegisterDifferentContentConflicts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.Register(ctx, sampleRollout([]Entry{{Key: "a", Value: "1"}})); err != nil {
		t.Fatalf("register: %v", err)
	}
	conflict := sampleRollout([]Entry{{Key: "a", Value: "2"}})
	if _, err := st.Register(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}

	listed, err := st.List(ctx, "staging", "billing", "payments")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Entries[0].Value != "1" {
		t.Fatal("conflict must leave the original record intact")
	}
}

func TestListOrdersByVersionAndFiltersIdentity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for _, version := range []int64{3, 1, 2} {
		rollout := sampleRollout([]Entry{{Key: "v", Value: fmt.Sprintf("%d", version)}})
		rollout.Version = version
		if _, err := st.Register(ctx, rollout); err != nil {
			t.Fatalf("register v%d: %v", version, err)
		}
	}
	other := sampleRollout([]Entry{{Key: "v", Value: "x"}})
	other.ConfigName = "other-config"
	if _, err := st.Register(ctx, other); err != nil {
		t.Fatalf("register other: %v", err)
	}

	listed, err := st.List(ctx, "staging", "billing", "payments")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("len = %d, want 3", len(listed))
	}
	for index, want := range []int64{1, 2, 3} {
		if listed[index].Version != want {
			t.Fatalf("position %d version = %d, want %d", index, listed[index].Version, want)
		}
	}

	missing, err := st.List(ctx, "prod", "billing", "payments")
	if err != nil {
		t.Fatalf("list missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("no match must return empty result, got %v", missing)
	}
}

func TestConcurrentRegisterSameIdentity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const goroutines = 12
	var wg sync.WaitGroup
	results := make(chan string, goroutines)
	errs := make(chan error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			rollout := sampleRollout([]Entry{{Key: "a", Value: "1"}})
			if i%2 == 1 {
				rollout.Entries = []Entry{{Key: "a", Value: "1"}}
			}
			stored, err := st.Register(ctx, rollout)
			if err != nil {
				errs <- err
				return
			}
			results <- stored.Digest
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent register: %v", err)
	}
	var digest string
	count := 0
	for got := range results {
		if digest == "" {
			digest = got
		} else if got != digest {
			t.Fatal("identical concurrent submissions must share one record")
		}
		count++
	}
	if count != goroutines {
		t.Fatalf("results = %d, want %d", count, goroutines)
	}
	listed, err := st.List(ctx, "staging", "billing", "payments")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(listed))
	}
}

func mustReopen(t *testing.T, current *Store) *Store {
	t.Helper()
	if err := current.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st, err := Open(current.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
