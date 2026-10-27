package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/wool"
)

// TestFileLoggerWritesLevelNames covers the fix for numeric level codes: a
// JSON log line must carry "DEBUG", not "2", so the file is readable without a
// level-number lookup table.
func TestFileLoggerWritesLevelNames(t *testing.T) {
	dir := t.TempDir()
	fl, err := NewFileLogger(dir)
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	defer fl.Close()

	fl.Process(&wool.Log{Level: wool.DEBUG, Message: "computing hash"})

	entry := readLastEntry(t, fl.Path())
	if entry.Level != "DEBUG" {
		t.Fatalf("expected level name DEBUG, got %q", entry.Level)
	}
	if entry.Message != "computing hash" {
		t.Fatalf("unexpected message: %q", entry.Message)
	}
}

func readLastEntry(t *testing.T, path string) fileEntry {
	t.Helper()
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer f.Close()

	var last string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			last = line
		}
	}
	if last == "" {
		t.Fatalf("log file %s is empty", path)
	}
	var entry fileEntry
	if err := json.Unmarshal([]byte(last), &entry); err != nil {
		t.Fatalf("unmarshal entry %q: %v", last, err)
	}
	return entry
}

func TestIndependentInvocationsKeepTheirOwnLogFiles(t *testing.T) {
	root := t.TempDir()
	old, err := NewFileLogger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	old.Process(&wool.Log{Level: wool.INFO, Message: "old preparation"})
	fresh, err := NewFileLogger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	fresh.Process(&wool.Log{Level: wool.INFO, Message: "fresh startup"})
	old.Process(&wool.Log{Level: wool.INFO, Message: "old shutdown"})
	if old.Path() == fresh.Path() {
		t.Fatal("independent invocations share the same backing file")
	}
	data, err := os.ReadFile(fresh.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old ") || !strings.Contains(string(data), "fresh startup") {
		t.Fatalf("new invocation contains another run's records: %s", data)
	}
	// Rapid repeated rotations must remain in the old invocation, never reuse
	// a shared date/second segment or change the fresh invocation's file.
	old.maxSize = 1
	paths := map[string]bool{old.Path(): true}
	for range 3 {
		old.Process(&wool.Log{Level: wool.INFO, Message: "old rotated shutdown"})
		if paths[old.Path()] {
			t.Fatal("rotation reused a full log segment")
		}
		paths[old.Path()] = true
	}
	updated, err := os.ReadFile(fresh.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) != string(data) {
		t.Fatal("old rotation changed the fresh invocation's log")
	}
}

func TestClosedFileLoggerNeverReopensOrChangesItsReportedPath(t *testing.T) {
	fl, err := NewFileLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl.Process(&wool.Log{Level: wool.INFO, Message: "final shutdown"})
	path := fl.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fl.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	fl.maxSize = 1
	fl.Process(&wool.Log{Level: wool.INFO, Message: "late record"})
	if fl.Path() != path {
		t.Fatal("closed logger reopened on rotation")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("closed logger accepted a late record")
	}
}
