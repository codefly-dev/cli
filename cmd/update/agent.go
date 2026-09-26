package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/shared"
	"gopkg.in/yaml.v3"
)

// agentUpdate records a resolved agent version bump for reporting.
type agentUpdate struct {
	Name string
	From string
	To   string
}

// inspectAgent starts a candidate agent and checks its live protocol, without
// invoking any lifecycle RPC. It is the one admission every selection this
// package writes goes through, and it is how a pinned version is proven to
// exist: the candidate is downloaded from its published release, so a version
// that was never published fails here, before any manifest is touched. Tests
// replace it to keep the host boundary off the network.
var inspectAgent = func(ctx context.Context, candidate *resources.Agent) error {
	inspectCtx, cancel := context.WithTimeout(ctx, manager.DefaultStartupTimeout+2*manager.DefaultDialTimeout)
	defer cancel()
	_, _, err := services.InspectAgent(inspectCtx, candidate)
	return err
}

// admissionError names what failed when a candidate agent is not admitted.
// Core's loader reports a missing release as manager.ErrAgentBinaryNotFound
// without the version it looked for, and asks callers to pick the message by
// that sentinel; for a pinned version, "not published" is the whole answer.
func admissionError(candidate *resources.Agent, err error) error {
	if errors.Is(err, manager.ErrAgentBinaryNotFound) {
		return &unpublishedAgentError{agent: candidate.Publisher + "/" + candidate.Name, version: candidate.Version, cause: err}
	}
	return err
}

// unpublishedAgentError says a pinned version has no downloadable release. It
// carries its cause for errors.Is but exposes it only as a multi-error, so the
// terminal's root-cause line (which follows single unwraps) is this message,
// the one that names the version, rather than the loader's.
type unpublishedAgentError struct {
	agent   string
	version string
	cause   error
}

func (e *unpublishedAgentError) Error() string {
	return fmt.Sprintf("agent %s is not published at version %s — no release could be downloaded; list its published versions with `codefly agent versions %s` (%v)",
		e.agent, e.version, e.agent, e.cause)
}

func (e *unpublishedAgentError) Unwrap() []error { return []error{e.cause} }

// updateServiceAgent checks the latest candidate's live protocol before updating
// a service's selection. Operation and functional qualification remain separate.
// Returns nil when nothing changed.
func updateServiceAgent(ctx context.Context, svc *resources.Service) (*agentUpdate, error) {
	if svc.Agent == nil {
		return nil, fmt.Errorf("service %s declares no agent", svc.Name)
	}
	candidate := *svc.Agent
	if _, err := manager.PinToLatestRelease(ctx, &candidate); err != nil {
		return nil, fmt.Errorf("cannot resolve latest agent version: %w", err)
	}
	return selectServiceAgent(ctx, svc, &candidate)
}

// pinServiceAgent selects exactly version for the service's agent — a release
// or a prerelease such as a `codefly publish dev` build — after the same
// admission a latest-release update takes. Returns nil when nothing changed.
func pinServiceAgent(ctx context.Context, svc *resources.Service, version string) (*agentUpdate, error) {
	if svc.Agent == nil {
		return nil, fmt.Errorf("service %s declares no agent", svc.Name)
	}
	exact, err := exactAgentVersion(version)
	if err != nil {
		return nil, err
	}
	candidate := *svc.Agent
	candidate.Version = exact
	return selectServiceAgent(ctx, svc, &candidate)
}

// exactAgentVersion accepts only an exact semantic version — the spelling
// core's agent-overrides parser accepts, prereleases included (0.1.47,
// 0.1.47-dev.abc123def456) — and refuses ranges, "latest", partial versions
// and a leading v, so a pin never reads as moved while resolving elsewhere.
func exactAgentVersion(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if _, err := semver.StrictNewVersion(trimmed); err != nil {
		return "", fmt.Errorf("agent version %q is not an exact semantic version (e.g. 0.1.47, or a dev build such as 0.1.47-dev.abc123def456)", version)
	}
	return trimmed, nil
}

// selectServiceAgent admits candidate and writes its version into the
// service's own service.codefly.yaml. It uses a surgical, text-preserving edit
// of the single agent.version token and never reserializes resources.Service,
// so unmodeled keys, comments, and formatting survive byte-for-byte.
func selectServiceAgent(ctx context.Context, svc *resources.Service, candidate *resources.Agent) (*agentUpdate, error) {
	file := filepath.Join(svc.Dir(), resources.ServiceConfigurationName)
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", file, err)
	}
	// Compare against the version the file declares, not the in-memory one: a
	// module loaded from a workspace reference carries the workspace's
	// agent-overrides, which never change this file.
	declared, err := resources.LoadServiceFromDir(ctx, svc.Dir())
	if err != nil {
		return nil, fmt.Errorf("cannot load %s: %w", file, err)
	}
	if declared.Agent == nil {
		return nil, fmt.Errorf("%s declares no agent", file)
	}
	from := declared.Agent.Version
	if candidate.Version == from {
		return nil, nil
	}
	if err = inspectAgent(ctx, candidate); err != nil {
		return nil, fmt.Errorf("cannot select agent %s: %w", candidate.Identifier(), admissionError(candidate, err))
	}
	updated, err := rewriteAgentVersion(content, candidate.Version)
	if err != nil {
		return nil, fmt.Errorf("cannot update %s: %w", file, err)
	}
	// Atomic write (temp + fsync + rename) so a crash mid-write cannot leave a
	// truncated service.codefly.yaml — the very data integrity this command
	// exists to protect.
	if err = shared.WriteFileAtomic(ctx, file, updated, 0o600); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", file, err)
	}
	svc.Agent.Version = candidate.Version
	return &agentUpdate{Name: svc.Agent.Name, From: from, To: svc.Agent.Version}, nil
}

// rewriteAgentVersion replaces only the agent.version scalar, editing the one
// source line it lives on and leaving every other byte — indentation, comments,
// unmodeled keys, quoting — exactly as written. The document is parsed only to
// locate the version node; it is never re-serialized (which would reflow the
// whole file to yaml.v3's canonical style).
func rewriteAgentVersion(content []byte, version string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse service configuration: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty service configuration")
	}
	agent := mappingValue(doc.Content[0], "agent")
	if agent == nil {
		return nil, fmt.Errorf("service configuration has no agent block")
	}
	versionNode := mappingValue(agent, "version")
	if versionNode == nil {
		return nil, fmt.Errorf("agent block has no version")
	}
	old := versionNode.Value
	if old == version {
		return content, nil
	}

	lines := bytes.Split(content, []byte("\n"))
	idx := versionNode.Line - 1
	if idx < 0 || idx >= len(lines) {
		return nil, fmt.Errorf("agent version node points outside the file")
	}
	line := lines[idx]
	// Column is 1-based and marks where the scalar token starts; search from
	// there so a quoted value ("0.0.22") and any coincidental earlier match are
	// both handled correctly.
	col := versionNode.Column - 1
	if col < 0 || col > len(line) {
		return nil, fmt.Errorf("agent version node points outside its line")
	}
	rel := bytes.Index(line[col:], []byte(old))
	if rel < 0 {
		return nil, fmt.Errorf("agent version value %q not found on its line", old)
	}
	at := col + rel
	lines[idx] = slices.Concat(line[:at], []byte(version), line[at+len(old):])
	return bytes.Join(lines, []byte("\n")), nil
}

// mappingValue returns the value node for key in a YAML mapping, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
