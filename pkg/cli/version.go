package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/cliupdate"
	"github.com/codefly-dev/core/releaseupdate"
)

var (
	noticeMu        sync.Mutex
	noticeFn        = func(msg string) { Warning("%s", msg) }
	updateCheckOnce sync.Once
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
	if !cliupdate.CurrentBuildInfo().Released() {
		return
	}
	updateCheckOnce.Do(func() {
		go checkForCLIForUpdate()
	})
}

func checkForCLIForUpdate() {
	service, err := cliupdate.NewService()
	if err != nil {
		return
	}
	check, err := service.BeginAutomaticCheck(time.Now(), 24*time.Hour)
	if err != nil || check == nil {
		return
	}
	defer func() { _ = check.Cancel() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := service.Check(ctx, releaseupdate.ChannelStable, false)
	notifiedVersion := ""
	if err == nil && result.Available && check.NotificationDue(result.Latest) {
		emitUpdateNotice("%s", result.Notice())
		notifiedVersion = result.Latest
	}
	completeCtx, completeCancel := context.WithTimeout(context.Background(), time.Second)
	defer completeCancel()
	_ = check.Complete(completeCtx, time.Now(), notifiedVersion)
}

func GetCurrentVersion() (string, error) {
	return cliupdate.CurrentBuildInfo().Version, nil
}

func GetBuildInfo() cliupdate.BuildInfo {
	return cliupdate.CurrentBuildInfo()
}
