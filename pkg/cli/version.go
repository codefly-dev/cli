package cli

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

type Information struct {
	Version string `json:"version"`
}

type Release struct {
	TagName string `json:"tag_name"`
}

func CheckForCLIForUpdate() {
	go checkForCLIForUpdate()
}

func checkForCLIForUpdate() {
	latest, err := getLatestRelease()
	if errors.Is(err, NoInternetError{}) {
		return
	} else if err != nil {
		Warning("Cannot get latest release for CLI")
		return
	}
	current, err := GetCurrentVersion()
	if err != nil {
		Warning("Cannot get current version for CLI")
		return
	}
	if fmt.Sprintf("v%s", current) != latest {
		Warning("A new version of codefly is available. Please update to %s", latest)
	}
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
