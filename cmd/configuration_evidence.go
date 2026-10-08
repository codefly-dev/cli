package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	buildinfo "runtime/debug"
	"time"
)

// configurationEvidence identifies the collection and producer, not the full
// resolver input closure. Consumers must not treat it as per-key provenance or
// match a saved report to current source using the workspace name alone.
type configurationEvidence struct {
	StartedAt        string `json:"started_at"`
	CompletedAt      string `json:"completed_at"`
	ExecutableSHA256 string `json:"executable_sha256,omitempty"`
	CoreVersion      string `json:"core_version,omitempty"`
	CoreReplaced     bool   `json:"core_replaced"`
	InputBinding     string `json:"input_binding"`
	Coverage         string `json:"coverage"`
}

func executableDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func newConfigurationEvidence() (*configurationEvidence, error) {
	e := &configurationEvidence{
		StartedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		InputBinding: "unavailable",
		Coverage:     "composed-group-key-inventory",
	}
	if info, ok := buildinfo.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/codefly-dev/core" {
				e.CoreVersion = dep.Version
				if dep.Replace != nil {
					e.CoreReplaced = true
					e.CoreVersion = dep.Replace.Version
				}
				break
			}
		}
	}
	path, err := os.Executable()
	if err != nil {
		return e, err
	}
	e.ExecutableSHA256, err = executableDigest(path)
	return e, err
}

// MarshalJSON preserves protobuf's defined JSON representation inside the
// existing doctor envelope. Explicit false booleans distinguish intermediate
// decisions and absent profiles for consumers of this versioned report.
func (r workspaceReadinessReport) MarshalJSON() ([]byte, error) {
	type envelope workspaceReadinessReport
	profiles, err := evidenceJSON(r.ConfigurationProfiles)
	if err != nil {
		return nil, err
	}
	decisions, err := evidenceJSON(r.ConfigurationDecisions)
	if err != nil {
		return nil, err
	}
	origins, err := evidenceJSON(r.ConfigurationOrigins)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		*envelope
		Profiles  []json.RawMessage `json:"configuration_profiles,omitempty"`
		Decisions []json.RawMessage `json:"configuration_decisions,omitempty"`
		Origins   []json.RawMessage `json:"configuration_origins,omitempty"`
	}{envelope: (*envelope)(&r), Profiles: profiles, Decisions: decisions, Origins: origins})
}

func evidenceJSON[T proto.Message](messages []T) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(messages))
	options := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
	for _, message := range messages {
		encoded, err := options.Marshal(message)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded)
	}
	return out, nil
}
