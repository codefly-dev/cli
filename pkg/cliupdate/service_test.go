package cliupdate

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/codefly-dev/core/releaseupdate"
)

type checkerFunc func(context.Context, releaseupdate.Request) (releaseupdate.Status, error)

func (check checkerFunc) Check(ctx context.Context, request releaseupdate.Request) (releaseupdate.Status, error) {
	return check(ctx, request)
}

func TestServiceCheckMapsCLIReleaseRequest(t *testing.T) {
	current := mustVersion(t, "1.2.3")
	latest := mustVersion(t, "1.3.0")
	checkedAt := time.Date(2026, 7, 28, 16, 23, 31, 0, time.UTC)
	var captured releaseupdate.Request
	service := &Service{
		build: releasedBuild,
		installation: Installation{
			Kind:         InstallKindDirect,
			ResolvedPath: "/usr/local/bin/codefly",
		},
		checker: checkerFunc(func(_ context.Context, request releaseupdate.Request) (releaseupdate.Status, error) {
			captured = request
			return releaseupdate.Status{
				Current:   current,
				Latest:    latest,
				Available: true,
				Release: releaseupdate.Release{
					Version: latest,
					URL:     "https://github.com/codefly-dev/cli/releases/tag/v1.3.0",
					Asset:   releaseupdate.Asset{Name: "codefly_1.3.0_darwin_arm64.tar.gz"},
				},
				CheckedAt: checkedAt,
			}, nil
		}),
	}

	result, err := service.Check(context.Background(), releaseupdate.ChannelBeta, true)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Repository != (releaseupdate.Repository{Owner: "codefly-dev", Name: "cli"}) {
		t.Fatalf("repository = %#v", captured.Repository)
	}
	if captured.Current.String() != "1.2.3" ||
		captured.Channel != releaseupdate.ChannelBeta ||
		captured.Downgrade != releaseupdate.DowngradeAllow ||
		captured.InstallKind != releaseupdate.InstallKindDirect ||
		captured.ArtifactName != "codefly" ||
		captured.Platform != (releaseupdate.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}) {
		t.Fatalf("request = %#v", captured)
	}
	if !result.Available || result.Latest != "1.3.0" || result.Asset == "" || result.CheckedAt != checkedAt.Format(time.RFC3339) {
		t.Fatalf("result = %#v", result)
	}
}

func TestServiceCheckUsesRateLimitedCacheOnlyWhenPresent(t *testing.T) {
	latest := mustVersion(t, "1.3.0")
	rateLimited := &releaseupdate.RateLimitError{}
	service := &Service{
		build:        releasedBuild,
		installation: Installation{Kind: InstallKindHomebrew},
		checker: checkerFunc(func(context.Context, releaseupdate.Request) (releaseupdate.Status, error) {
			return releaseupdate.Status{Latest: latest, FromCache: true, Stale: true}, rateLimited
		}),
	}

	result, err := service.Check(context.Background(), releaseupdate.ChannelStable, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Warning == "" || !result.FromCache || !result.Stale {
		t.Fatalf("cached rate-limit result = %#v", result)
	}

	service.checker = checkerFunc(func(context.Context, releaseupdate.Request) (releaseupdate.Status, error) {
		return releaseupdate.Status{}, rateLimited
	})
	if _, err := service.Check(context.Background(), releaseupdate.ChannelStable, false); !errors.Is(err, releaseupdate.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
}

func TestServiceCheckDevelopmentBuildDoesNotUseReleaseSource(t *testing.T) {
	service := &Service{
		build:        BuildInfo{Version: "development"},
		installation: Installation{Kind: InstallKindDevelopment},
		checker: checkerFunc(func(context.Context, releaseupdate.Request) (releaseupdate.Status, error) {
			t.Fatal("development build queried release source")
			return releaseupdate.Status{}, nil
		}),
	}

	result, err := service.Check(context.Background(), releaseupdate.ChannelStable, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Current != "development" || result.Available {
		t.Fatalf("result = %#v", result)
	}
}

func mustVersion(t *testing.T, value string) releaseupdate.Version {
	t.Helper()
	version, err := releaseupdate.ParseVersion(value)
	if err != nil {
		t.Fatal(err)
	}
	return version
}
