package cli

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"

	"gopkg.in/yaml.v2"
)

type Information struct {
	Version string `json:"version"`
}

type Release struct {
	TagName string `json:"tag_name"`
}

func CheckForCLIForUpdate() {
	latest, err := getLatestRelease()
	ExitOnError(err, "Cannot get latest release for CLI")
	current, err := GetCurrentVersion()
	ExitOnError(err, "Cannot get current version for CLI")
	if fmt.Sprintf("v%s", current) != latest {
		Warning("A new version of codefly is available. Please update to %s", latest)
	}
}

func getLatestRelease() (string, error) {
	resp, err := http.Get("https://api.github.com/repos/codefly-dev/cli-releases/releases/latest")
	if err != nil {
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
		log.Fatalf("error: %v", err)
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
