package cliupdate

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/codefly-dev/core/releaseupdate"
	"github.com/gofrs/flock"
)

func TestStateStorePersistsCacheAndAutomaticCadence(t *testing.T) {
	store := NewStateStore(t.TempDir())
	version, err := releaseupdate.ParseVersion("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	entry := releaseupdate.CacheEntry{
		Metadata: releaseupdate.CacheMetadata{
			ETag:        `"release-etag"`,
			ValidatedAt: time.Date(2026, 7, 28, 16, 23, 31, 0, time.UTC),
		},
		Releases: []releaseupdate.Release{{Version: version}},
	}
	if err := store.Save(context.Background(), "stable", entry); err != nil {
		t.Fatal(err)
	}

	loaded, found, err := NewStateStore(store.directory).Load(context.Background(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	if !found || loaded.Metadata.ETag != entry.Metadata.ETag || loaded.Releases[0].Version.String() != "1.2.3" {
		t.Fatalf("loaded cache = %#v, found %v", loaded, found)
	}
	info, err := os.Stat(store.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	got, err := store.BeginAutomaticCheck(now, 24*time.Hour)
	assertMutationResult(t, got, err, true)
	got, err = store.BeginAutomaticCheck(now.Add(23*time.Hour), 24*time.Hour)
	assertMutationResult(t, got, err, false)
	got, err = store.BeginAutomaticCheck(now.Add(24*time.Hour), 24*time.Hour)
	assertMutationResult(t, got, err, true)
	got, err = store.MarkNotified("1.2.4")
	assertMutationResult(t, got, err, true)
	got, err = store.MarkNotified("1.2.4")
	assertMutationResult(t, got, err, false)
	got, err = store.MarkNotified("1.2.5")
	assertMutationResult(t, got, err, true)

	if _, found, err := store.Load(context.Background(), "stable"); err != nil || !found {
		t.Fatalf("cache was not preserved after automatic state writes: found %v, err %v", found, err)
	}
}

func TestStateStoreAutomaticMutationDoesNotWaitForLock(t *testing.T) {
	store := NewStateStore(t.TempDir())
	if err := store.prepare(); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(store.lockPath)
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()

	got, err := store.BeginAutomaticCheck(time.Now(), time.Hour)
	assertMutationResult(t, got, err, false)
}

func assertMutationResult(t *testing.T, got bool, err error, want bool) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("mutation result = %v, want %v", got, want)
	}
}
