package killedrun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReportIsPublishedInTheTestDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report")
	if err := writeReport(path, "12 34"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "12 34" {
		t.Fatalf("report = %q, %v", got, err)
	}
	if _, err = os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary report remains: %v", err)
	}
}

func TestReportCannotFollowASymlinkOutsideTheTestDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "protected")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "report.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(filepath.Join(dir, "report"), "overwrite"); err == nil {
		t.Fatal("outside write admitted")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "unchanged" {
		t.Fatalf("outside content = %q, %v", got, err)
	}
}
