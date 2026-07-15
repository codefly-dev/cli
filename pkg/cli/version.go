package cli

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver"
	"gopkg.in/yaml.v2"
)

type Information struct {
	Version string `json:"version"`
}

type Release struct {
	TagName string `json:"tag_name"`
}

var (
	noticeMu sync.Mutex
	noticeFn = func(msg string) { Warning("%s", msg) }
)

// emitUpdateNotice delivers a message from the background update check to the
// active sink. It exists because the check runs in a goroutine that finishes
// at an arbitrary time — including mid-render while a TUI owns the terminal.
func emitUpdateNotice(format string, args ...any) {
	noticeMu.Lock()
	fn := noticeFn
	noticeMu.Unlock()
	fn(fmt.Sprintf(format, args...))
}

// CaptureUpdateNotice redirects the background update check's output away from
// stderr — where, firing mid-render, it corrupts Bubbletea's inline status bar
// and leaves a stale, duplicated footer (#57) — into sink. sink may be invoked
// from the update goroutine at any time, so it must be goroutine-safe. The
// returned func restores the default stderr delivery.
func CaptureUpdateNotice(sink func(msg string)) (restore func()) {
	noticeMu.Lock()
	prev := noticeFn
	noticeFn = sink
	noticeMu.Unlock()
	return func() {
		noticeMu.Lock()
		noticeFn = prev
		noticeMu.Unlock()
	}
}

func CheckForCLIForUpdate() {
	go checkForCLIForUpdate()
}

func checkForCLIForUpdate() {
	latest, err := getLatestRelease()
	if errors.Is(err, NoInternetError{}) {
		return
	} else if err != nil {
		emitUpdateNotice("Cannot get latest release for CLI")
		return
	}
	current, err := GetCurrentVersion()
	if err != nil {
		emitUpdateNotice("Cannot get current version for CLI")
		return
	}
	newer, err := isNewerVersion(current, latest)
	if err != nil {
		emitUpdateNotice("Cannot compare CLI release versions")
		return
	}
	if newer {
		emitUpdateNotice("A new version of codefly is available. Please update to %s", latest)
	}
}

func isNewerVersion(current, latest string) (bool, error) {
	currentVersion, err := semver.NewVersion(current)
	if err != nil {
		return false, fmt.Errorf("parse current version %q: %w", current, err)
	}
	latestVersion, err := semver.NewVersion(latest)
	if err != nil {
		return false, fmt.Errorf("parse latest version %q: %w", latest, err)
	}
	return latestVersion.GreaterThan(currentVersion), nil
}

type NoInternetError struct {
}

func (NoInternetError) Error() string {
	return "no internet"
}

func getLatestRelease() (string, error) {
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get("https://api.github.com/repos/codefly-dev/cli-releases/releases/latest")
	if err != nil {
		if strings.Contains(err.Error(), "i/o timeout") {
			return "", NoInternetError{}
		}
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var release Release
	err = json.Unmarshal(body, &release)
	if err != nil {
		return "", err
	}

	return release.TagName, nil
}

func GetCurrentVersion() (string, error) {
	data, err := fs.ReadFile(infoFS, "info.yaml")
	if err != nil {
		return "", fmt.Errorf("read embedded CLI version: %w", err)
	}

	// Unmarshal YAML into a struct
	var info Information
	err = yaml.Unmarshal(data, &info)
	if err != nil {
		return "", err
	}
	return info.Version, nil
}

//go:embed info.yaml
var infoFS embed.FS
