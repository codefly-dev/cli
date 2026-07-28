package cliupdate

import (
	"fmt"
	"strings"
	"time"

	"github.com/codefly-dev/core/releaseupdate"
)

var (
	version   = "development"
	commit    = "unknown"
	buildDate = "unknown"
)

type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

func CurrentBuildInfo() BuildInfo {
	return BuildInfo{
		Version:   strings.TrimSpace(version),
		Commit:    strings.TrimSpace(commit),
		BuildDate: strings.TrimSpace(buildDate),
	}
}

func (info BuildInfo) SemanticVersion() (releaseupdate.Version, error) {
	parsed, err := releaseupdate.ParseVersion(info.Version)
	if err != nil {
		return releaseupdate.Version{}, fmt.Errorf("Codefly build version is not a release version: %w", err)
	}
	return parsed, nil
}

func (info BuildInfo) Released() bool {
	if _, err := info.SemanticVersion(); err != nil {
		return false
	}
	if info.Commit == "" || info.Commit == "unknown" {
		return false
	}
	if info.BuildDate == "" || info.BuildDate == "unknown" {
		return false
	}
	_, err := time.Parse(time.RFC3339, info.BuildDate)
	return err == nil
}
