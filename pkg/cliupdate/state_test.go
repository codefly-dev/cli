package cliupdate

import (
	"context"
	"errors"
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
	check, err := store.BeginAutomaticCheck(now, 24*time.Hour)
	if err != nil || check == nil {
		t.Fatalf("begin automatic check = %#v, %v", check, err)
	}
	if !check.NotificationDue("1.2.4") {
		t.Fatal("new release notification was suppressed")
	}
	if err := check.Complete(context.Background(), now, "1.2.4"); err != nil {
		t.Fatal(err)
	}
	check, err = store.BeginAutomaticCheck(now.Add(23*time.Hour), 24*time.Hour)
	if err != nil || check != nil {
		t.Fatalf("check before cadence = %#v, %v", check, err)
	}
	check, err = store.BeginAutomaticCheck(now.Add(24*time.Hour), 24*time.Hour)
	if err != nil || check == nil {
		t.Fatalf("check at cadence = %#v, %v", check, err)
	}
	if check.NotificationDue("1.2.4") {
		t.Fatal("previously emitted release notification repeated")
	}
	if !check.NotificationDue("1.2.5") {
		t.Fatal("newer release notification was suppressed")
	}
	if err := check.Complete(context.Background(), now.Add(24*time.Hour), "1.2.5"); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Load(context.Background(), "stable"); err != nil || !found {
		t.Fatalf("cache was not preserved after automatic state writes: found %v, err %v", found, err)
	}
}

func TestStateStoreDoesNotPersistAbandonedAutomaticCheck(t *testing.T) {
	store := NewStateStore(t.TempDir())
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	check, err := store.BeginAutomaticCheck(now, 24*time.Hour)
	if err != nil || check == nil {
		t.Fatalf("begin automatic check = %#v, %v", check, err)
	}
	if err := check.Cancel(); err != nil {
		t.Fatal(err)
	}
	retry, err := store.BeginAutomaticCheck(now.Add(time.Minute), 24*time.Hour)
	if err != nil || retry == nil {
		t.Fatalf("abandoned check suppressed retry = %#v, %v", retry, err)
	}
	if err := retry.Cancel(); err != nil {
		t.Fatal(err)
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

	check, err := store.BeginAutomaticCheck(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if check != nil {
		_ = check.Cancel()
		t.Fatal("automatic check waited for state lock")
	}
}

func TestStateStoreLockWaitHonorsContext(t *testing.T) {
	store := NewStateStore(t.TempDir())
	if err := store.prepare(); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(store.lockPath)
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	unlocked := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = lock.Unlock()
		close(unlocked)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := store.Load(ctx, "stable")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("load error = %v, want context deadline", err)
	}
	<-unlocked
}
