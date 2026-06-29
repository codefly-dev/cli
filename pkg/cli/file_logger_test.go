package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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
