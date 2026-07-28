package cliupdate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/codefly-dev/core/releaseupdate"
	"github.com/codefly-dev/core/resources"
)

const (
	repositoryOwner = "codefly-dev"
	repositoryName  = "cli"
	artifactName    = "codefly"
	checksumsName   = "checksums.txt"
)

type CheckResult struct {
	SchemaVersion int    `json:"schema_version"`
	Current       string `json:"current"`
	Commit        string `json:"commit"`
	BuildDate     string `json:"build_date"`
	Latest        string `json:"latest,omitempty"`
	Available     bool   `json:"available"`
	Channel       string `json:"channel"`
	InstallKind   string `json:"install_kind"`
	ReleaseURL    string `json:"release_url,omitempty"`
	PublishedAt   string `json:"published_at,omitempty"`
	Asset         string `json:"asset,omitempty"`
	CheckedAt     string `json:"checked_at,omitempty"`
	FromCache     bool   `json:"from_cache"`
	Stale         bool   `json:"stale"`
	Action        string `json:"action"`
	Warning       string `json:"warning,omitempty"`

	status releaseupdate.Status
}

type Service struct {
	build        BuildInfo
	installation Installation
	checker      releaseupdate.Checker
	installer    releaseupdate.Installer
	state        *StateStore
	stagingDir   string
}

//go:embed release-signing-cert.pem
var trustFS embed.FS

func NewService() (*Service, error) {
	certificate, err := trustFS.ReadFile("release-signing-cert.pem")
	if err != nil {
		return nil, fmt.Errorf("read release trust root: %w", err)
	}
	installation, err := DetectInstallation()
	if err != nil {
		return nil, err
	}
	stateDirectory := filepath.Join(resources.CodeflyHomeDir(), "update")
	state := NewStateStore(stateDirectory)
	client := &http.Client{Timeout: 30 * time.Second}
	checker, err := releaseupdate.NewGitHubChecker(releaseupdate.GitHubOptions{
		HTTPClient: client,
		UserAgent:  "codefly-cli-update",
		Store:      state,
	})
	if err != nil {
		return nil, fmt.Errorf("configure release checker: %w", err)
	}
	installer, err := releaseupdate.NewInstaller(releaseupdate.InstallerOptions{
		HTTPClient:              client,
		PublisherCertificatePEM: certificate,
		ChecksumsAssetName:      checksumsName,
		AllowedDownloadHosts: []string{
			"api.github.com",
			"github.com",
			"objects.githubusercontent.com",
			"release-assets.githubusercontent.com",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure release installer: %w", err)
	}
	return &Service{
		build:        CurrentBuildInfo(),
		installation: installation,
		checker:      checker,
		installer:    installer,
		state:        state,
		stagingDir:   filepath.Join(stateDirectory, "staging"),
	}, nil
}

func (service *Service) Installation() Installation {
	return service.installation
}

func (service *Service) Check(ctx context.Context, channel releaseupdate.Channel, allowDowngrade bool) (CheckResult, error) {
	if channel != releaseupdate.ChannelStable && channel != releaseupdate.ChannelBeta {
		return CheckResult{}, fmt.Errorf("%w: %q", releaseupdate.ErrUnsupportedChannel, channel)
	}
	result := CheckResult{
		SchemaVersion: 1,
		Current:       service.build.Version,
		Commit:        service.build.Commit,
		BuildDate:     service.build.BuildDate,
		Channel:       string(channel),
		InstallKind:   string(service.installation.Kind),
		Action:        service.installation.Action(),
	}
	current, err := service.build.SemanticVersion()
	if err != nil {
		if service.build.Version == "development" {
			return result, nil
		}
		return CheckResult{}, err
	}
	downgrade := releaseupdate.DowngradeDisallow
	if allowDowngrade {
		downgrade = releaseupdate.DowngradeAllow
	}
	status, checkErr := service.checker.Check(ctx, releaseupdate.Request{
		Repository:   releaseupdate.Repository{Owner: repositoryOwner, Name: repositoryName},
		Current:      current,
		Channel:      channel,
		Platform:     releaseupdate.CurrentPlatform(),
		ArtifactName: artifactName,
		InstallKind:  service.installation.CoreKind(),
		Downgrade:    downgrade,
	})
	if checkErr != nil && !errors.Is(checkErr, releaseupdate.ErrRateLimited) {
		return CheckResult{}, checkErr
	}
	if errors.Is(checkErr, releaseupdate.ErrRateLimited) && status.Latest.String() == "" {
		return CheckResult{}, checkErr
	}
	result.Latest = status.Latest.String()
	result.Available = status.Available
	result.ReleaseURL = status.Release.URL
	result.Asset = status.Release.Asset.Name
	result.PublishedAt = formatTime(status.Release.PublishedAt)
	result.CheckedAt = formatTime(status.CheckedAt)
	result.FromCache = status.FromCache
	result.Stale = status.Stale
	result.status = status
	if checkErr != nil {
		result.Warning = "GitHub rate limited the check; cached release metadata is shown."
	}
	return result, nil
}

func (service *Service) StageAndApply(ctx context.Context, result CheckResult) (releaseupdate.ApplyResult, error) {
	if service.installation.Kind != InstallKindDirect {
		return releaseupdate.ApplyResult{}, releaseupdate.ErrInstallNotOwned
	}
	if !result.Available {
		return releaseupdate.ApplyResult{}, releaseupdate.ErrUpdateNotAvailable
	}
	if err := os.MkdirAll(service.stagingDir, 0o700); err != nil {
		return releaseupdate.ApplyResult{}, fmt.Errorf("create update staging directory: %w", err)
	}
	staged, err := service.installer.StageAndVerify(ctx, result.status, releaseupdate.Destination{
		Path:             service.installation.ResolvedPath,
		StagingDirectory: service.stagingDir,
		InstallKind:      releaseupdate.InstallKindDirect,
	})
	if err != nil {
		return releaseupdate.ApplyResult{}, err
	}
	defer func() { _ = staged.Discard() }()
	return staged.Apply(ctx)
}

func (service *Service) BeginAutomaticCheck(now time.Time, cadence time.Duration) (bool, error) {
	return service.state.BeginAutomaticCheck(now, cadence)
}

func (service *Service) MarkNotified(version string) (bool, error) {
	return service.state.MarkNotified(version)
}

func (result CheckResult) Notice() string {
	if !result.Available {
		return ""
	}
	return fmt.Sprintf("Codefly v%s is available. %s", result.Latest, result.Action)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
