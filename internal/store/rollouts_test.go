package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleRollout() Rollout {
	return Rollout{
		ConfigName:        "billing",
		TargetEnvironment: "prod",
		Namespace:         "payments",
		Version:           3,
		Entries: []Entry{
			{Key: "zeta", Value: "9"},
			{Key: "alpha", Value: "1"},
		},
	}
}

func TestCreateRolloutStoresCanonicalRecord(t *testing.T) {
	st := openTemp(t)

	stored, created, err := st.CreateRollout(context.Background(), sampleRollout())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if stored.Revoked {
		t.Fatal("revoked = true, want false")
	}
	if len(stored.Digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", stored.Digest)
	}
	if stored.Entries[0].Key != "alpha" || stored.Entries[1].Key != "zeta" {
		t.Fatalf("entries not sorted by key: %+v", stored.Entries)
	}
}

func TestDigestIgnoresEntryOrder(t *testing.T) {
	first := []Entry{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}
	second := []Entry{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}}
	if DigestEntries(first) != DigestEntries(second) {
		t.Fatal("digests differ for the same key/value set")
	}
}

func TestDigestChangesWithAnyKeyOrValue(t *testing.T) {
	base := []Entry{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}
	variants := [][]Entry{
		{{Key: "a", Value: "1"}, {Key: "b", Value: "3"}},
		{{Key: "a", Value: "1"}, {Key: "c", Value: "2"}},
		{{Key: "a", Value: "1"}},
		{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}},
	}
	for i, variant := range variants {
		if DigestEntries(base) == DigestEntries(variant) {
			t.Fatalf("variant %d keeps the base digest", i)
		}
	}
}

func TestDuplicateCreateReturnsExistingRecord(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	first, created, err := st.CreateRollout(ctx, sampleRollout())
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	second, created, err := st.CreateRollout(ctx, sampleRollout())
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if created {
		t.Fatal("created = true, want false for identical resubmission")
	}
	if second.Digest != first.Digest {
		t.Fatalf("digest changed across duplicate: %q vs %q", first.Digest, second.Digest)
	}
	items, err := st.ListRollouts(ctx, "prod", "payments", "billing")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
}

func TestConflictingCreateIsRejected(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	if _, _, err := st.CreateRollout(ctx, sampleRollout()); err != nil {
		t.Fatalf("create: %v", err)
	}
	conflicting := sampleRollout()
	conflicting.Entries = []Entry{{Key: "alpha", Value: "changed"}}
	if _, _, err := st.CreateRollout(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	items, err := st.ListRollouts(ctx, "prod", "payments", "billing")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0].Digest != DigestEntries(sampleRollout().Entries) {
		t.Fatal("stored record was modified by the conflicting submission")
	}
}

func TestListRolloutsOrdersByVersionAndScopes(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	for _, version := range []int64{5, 1, 3} {
		rollout := sampleRollout()
		rollout.Version = version
		if _, _, err := st.CreateRollout(ctx, rollout); err != nil {
			t.Fatalf("create version %d: %v", version, err)
		}
	}
	other := sampleRollout()
	other.Namespace = "other"
	if _, _, err := st.CreateRollout(ctx, other); err != nil {
		t.Fatalf("create other namespace: %v", err)
	}

	items, err := st.ListRollouts(ctx, "prod", "payments", "billing")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	for i, want := range []int64{1, 3, 5} {
		if items[i].Version != want {
			t.Fatalf("items[%d].Version = %d, want %d", i, items[i].Version, want)
		}
		if items[i].Entries[0].Key != "alpha" {
			t.Fatalf("items[%d] entries not sorted by key: %+v", i, items[i].Entries)
		}
	}

	missing, err := st.ListRollouts(ctx, "staging", "payments", "billing")
	if err != nil {
		t.Fatalf("list missing: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("len(missing) = %d, want 0", len(missing))
	}
}

func TestConcurrentIdenticalCreatesKeepSingleRecord(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := st.CreateRollout(ctx, sampleRollout())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}

	items, err := st.ListRollouts(ctx, "prod", "payments", "billing")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
}

func TestConcurrentConflictingCreatesKeepOneRecord(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	contents := []Rollout{sampleRollout(), sampleRollout(), sampleRollout()}
	contents[1].Entries = []Entry{{Key: "alpha", Value: "one"}}
	contents[2].Entries = []Entry{{Key: "alpha", Value: "two"}}

	var wg sync.WaitGroup
	results := make(chan error, len(contents))
	for _, rollout := range contents {
		wg.Add(1)
		go func(rollout Rollout) {
			defer wg.Done()
			_, _, err := st.CreateRollout(ctx, rollout)
			results <- err
		}(rollout)
	}
	wg.Wait()
	close(results)

	conflicts := 0
	for err := range results {
		if errors.Is(err, ErrConflict) {
			conflicts++
		} else if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}
	if conflicts != len(contents)-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, len(contents)-1)
	}
	items, err := st.ListRollouts(ctx, "prod", "payments", "billing")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
}
