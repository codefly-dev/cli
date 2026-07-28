package cliupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codefly-dev/core/releaseupdate"
	"github.com/gofrs/flock"
)

const (
	stateSchemaVersion = 1
	lockRetryDelay     = 10 * time.Millisecond
)

type StateStore struct {
	directory         string
	statePath         string
	lockPath          string
	automaticLockPath string
}

type persistedState struct {
	SchemaVersion int                                 `json:"schema_version"`
	Cache         map[string]releaseupdate.CacheEntry `json:"cache"`
	Automatic     automaticState                      `json:"automatic"`
}

type automaticState struct {
	LastAttempt  time.Time `json:"last_attempt,omitempty"`
	LastNotified string    `json:"last_notified,omitempty"`
}

type AutomaticCheck struct {
	store        *StateStore
	lock         *flock.Flock
	lastNotified string
	mutex        sync.Mutex
	closed       bool
}

func NewStateStore(directory string) *StateStore {
	return &StateStore{
		directory:         directory,
		statePath:         filepath.Join(directory, "state.json"),
		lockPath:          filepath.Join(directory, "state.lock"),
		automaticLockPath: filepath.Join(directory, "automatic-check.lock"),
	}
}

func (store *StateStore) Load(ctx context.Context, key string) (entry releaseupdate.CacheEntry, found bool, result error) {
	if err := ctx.Err(); err != nil {
		return releaseupdate.CacheEntry{}, false, err
	}
	result = store.withLock(ctx, false, func() error {
		state, err := store.read()
		if err != nil {
			return err
		}
		entry, found = state.Cache[key]
		return nil
	})
	return entry, found, result
}

func (store *StateStore) Save(ctx context.Context, key string, entry releaseupdate.CacheEntry) error { //nolint:gocritic // releaseupdate.Store fixes the value signature.
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.withLock(ctx, true, func() error {
		state, err := store.read()
		if err != nil {
			return err
		}
		state.Cache[key] = entry
		return store.write(state)
	})
}

func (store *StateStore) BeginAutomaticCheck(now time.Time, cadence time.Duration) (*AutomaticCheck, error) {
	if cadence <= 0 {
		return nil, errors.New("automatic update cadence must be positive")
	}
	if err := store.prepare(); err != nil {
		return nil, err
	}
	automaticLock := flock.New(store.automaticLockPath, flock.SetPermissions(0o600))
	locked, err := automaticLock.TryLock()
	if err != nil || !locked {
		return nil, err
	}

	stateLock := flock.New(store.lockPath, flock.SetPermissions(0o600))
	locked, err = stateLock.TryRLock()
	if err != nil || !locked {
		_ = automaticLock.Unlock()
		return nil, err
	}
	state, err := store.read()
	unlockErr := stateLock.Unlock()
	if err != nil {
		_ = automaticLock.Unlock()
		return nil, errors.Join(err, unlockErr)
	}
	if unlockErr != nil {
		_ = automaticLock.Unlock()
		return nil, unlockErr
	}

	now = now.UTC()
	last := state.Automatic.LastAttempt
	if !last.IsZero() && !last.After(now) && now.Sub(last) < cadence {
		return nil, automaticLock.Unlock()
	}
	return &AutomaticCheck{
		store:        store,
		lock:         automaticLock,
		lastNotified: state.Automatic.LastNotified,
	}, nil
}

func (check *AutomaticCheck) NotificationDue(version string) bool {
	return version != "" && version != check.lastNotified
}

func (check *AutomaticCheck) Complete(ctx context.Context, completedAt time.Time, notifiedVersion string) error {
	check.mutex.Lock()
	defer check.mutex.Unlock()
	if check.closed {
		return errors.New("automatic update check is already closed")
	}
	check.closed = true

	var result error
	if completedAt.IsZero() {
		result = errors.New("automatic update completion time is required")
	} else {
		result = check.store.withLock(ctx, true, func() error {
			state, err := check.store.read()
			if err != nil {
				return err
			}
			state.Automatic.LastAttempt = completedAt.UTC()
			if notifiedVersion != "" {
				state.Automatic.LastNotified = notifiedVersion
			}
			return check.store.write(state)
		})
	}
	return errors.Join(result, check.lock.Unlock())
}

func (check *AutomaticCheck) Cancel() error {
	check.mutex.Lock()
	defer check.mutex.Unlock()
	if check.closed {
		return nil
	}
	check.closed = true
	return check.lock.Unlock()
}

func (store *StateStore) withLock(ctx context.Context, exclusive bool, action func() error) error {
	if err := store.prepare(); err != nil {
		return err
	}
	lock := flock.New(store.lockPath, flock.SetPermissions(0o600))
	var (
		locked bool
		err    error
	)
	if exclusive {
		locked, err = lock.TryLockContext(ctx, lockRetryDelay)
	} else {
		locked, err = lock.TryRLockContext(ctx, lockRetryDelay)
	}
	if err != nil {
		return err
	}
	if !locked {
		return ctx.Err()
	}
	defer func() { _ = lock.Unlock() }()
	return action()
}

func (store *StateStore) prepare() error {
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return fmt.Errorf("create update state directory: %w", err)
	}
	return nil
}

func (store *StateStore) read() (persistedState, error) {
	state := persistedState{
		SchemaVersion: stateSchemaVersion,
		Cache:         make(map[string]releaseupdate.CacheEntry),
	}
	data, err := os.ReadFile(store.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("read update state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("decode update state: %w", err)
	}
	if state.SchemaVersion != stateSchemaVersion {
		return persistedState{}, fmt.Errorf("unsupported update state schema %d", state.SchemaVersion)
	}
	if state.Cache == nil {
		state.Cache = make(map[string]releaseupdate.CacheEntry)
	}
	return state, nil
}

func (store *StateStore) write(state persistedState) error {
	state.SchemaVersion = stateSchemaVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(store.directory, ".update-state-*")
	if err != nil {
		return fmt.Errorf("create update state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure update state: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write update state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync update state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close update state: %w", err)
	}
	if err := os.Rename(temporaryPath, store.statePath); err != nil {
		return fmt.Errorf("commit update state: %w", err)
	}
	return nil
}
