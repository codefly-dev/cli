// Package fixture reads what a native run takes from the host module's
// fixture and the workspace's environment: who administers the platform for
// this run, and which module grants the envelope carries.
package fixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// Administrator is the fixture's platform administrator: the one identity that
// writes the module envelope.
type Administrator struct {
	Provider string
	Token    string
}

// ReadAdministrator reads the super_admin of fixture under moduleDir/fixtures.
func ReadAdministrator(moduleDir, fixture string) (Administrator, error) {
	path := filepath.Join(moduleDir, "fixtures", fixture+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return Administrator{}, fmt.Errorf("the envelope is written by the fixture's platform administrator, and the host declares no fixture %q: %w", fixture, err)
	}
	var document struct {
		Users []struct {
			Role         string `yaml:"role"`
			Provider     string `yaml:"provider"`
			ProviderID   string `yaml:"provider_id"`
			FixtureToken string `yaml:"fixture_token"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Administrator{}, fmt.Errorf("read %s: %w", path, err)
	}
	for _, user := range document.Users {
		if user.Role != "super_admin" {
			continue
		}
		token := user.FixtureToken
		if token == "" {
			token = user.ProviderID
		}
		return Administrator{Provider: user.Provider, Token: token}, nil
	}
	return Administrator{}, fmt.Errorf("fixture %q seeds no super_admin, so nobody can write the envelope", fixture)
}

// ReadGrants reads configurations/<env>/module-envelope.json: each principal's
// grant, raw. No file is an empty envelope.
func ReadGrants(workspaceDir, environment string) (map[string]json.RawMessage, error) {
	path := grantsPath(workspaceDir, environment)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var document struct {
		Principals map[string]json.RawMessage `json:"principals"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return document.Principals, nil
}

// ReadMinimumRevision reads the top-level "minimum_revision" of a workspace's
// configurations/<env>/module-envelope.json: the lowest revision a delivered
// envelope may carry, 0 when the file states none. A stated floor is an
// integer of at least 1. A native run never reads it: it writes the envelope
// at the host's next revision, whatever the floor.
func ReadMinimumRevision(workspaceDir, environment string) (uint64, error) {
	path := grantsPath(workspaceDir, environment)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var document struct {
		MinimumRevision json.RawMessage `json:"minimum_revision"`
	}
	if err = json.Unmarshal(data, &document); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	if document.MinimumRevision == nil {
		return 0, nil
	}
	floor, err := strconv.ParseUint(string(document.MinimumRevision), 10, 64)
	if err != nil || floor == 0 {
		return 0, fmt.Errorf("%s: minimum_revision is %s; it is an integer of at least 1", path, document.MinimumRevision)
	}
	return floor, nil
}

// ReadCompositionMinimumRevision is the composition's revision floor, with
// the precedence grants have: the consuming workspace's when it states one;
// otherwise the one floor the composed workspaces stating it agree on. Two
// composed workspaces stating different floors, which the consuming
// workspace does not decide, are refused by name rather than resolved to
// either: a floor is a statement about the host's history, and only the
// workspace that publishes to it knows which history is true.
func ReadCompositionMinimumRevision(workspace *resources.Workspace, environment string) (uint64, error) {
	own, err := ReadMinimumRevision(workspace.Dir(), environment)
	if err != nil {
		return 0, err
	}
	children := composedWorkspacesOf(workspace)
	composed := make([]FloorSource, 0, len(children))
	for _, child := range children {
		floor, readErr := ReadMinimumRevision(child.Dir(), environment)
		if readErr != nil {
			return 0, readErr
		}
		composed = append(composed, FloorSource{Workspace: child.Name, Floor: floor})
	}
	return MergeMinimumRevision(FloorSource{Workspace: workspace.Name, Floor: own}, composed, environment)
}

// FloorSource is one workspace's minimum_revision, 0 when it states none.
type FloorSource struct {
	Workspace string
	Floor     uint64
}

// MergeMinimumRevision is the composition's floor from its sources (see
// ReadCompositionMinimumRevision).
func MergeMinimumRevision(own FloorSource, composed []FloorSource, environment string) (uint64, error) {
	if own.Floor > 0 {
		return own.Floor, nil
	}
	var decided FloorSource
	for _, source := range composed {
		switch {
		case source.Floor == 0 || source.Floor == decided.Floor:
		case decided.Floor == 0:
			decided = source
		default:
			return 0, fmt.Errorf("workspaces %q and %q state minimum_revision %d and %d, and workspace %q does not decide it: state it in %s/configurations/%s/module-envelope.json",
				decided.Workspace, source.Workspace, decided.Floor, source.Floor, own.Workspace, own.Workspace, environment)
		}
	}
	return decided.Floor, nil
}

// GrantSource is one workspace's envelope policy: the workspace that states it
// and the principals it grants.
type GrantSource struct {
	Workspace  string
	Principals map[string]json.RawMessage
}

// ReadCompositionGrants reads the composition's envelope policy. A composed
// workspace states the grants of the module principals it composes — a
// platform base states its modules' — and the consuming workspace states its
// own, a solution's principal among them; the consuming workspace wins per
// principal, which is the precedence configuration groups already have, so a
// solution composed on a platform adds its principal without restating the
// platform's. Two composed workspaces must agree on a principal the consuming
// workspace does not decide.
func ReadCompositionGrants(workspace *resources.Workspace, environment string) (map[string]json.RawMessage, error) {
	var composed []GrantSource
	for _, child := range composedWorkspacesOf(workspace) {
		principals, err := ReadGrants(child.Dir(), environment)
		if err != nil {
			return nil, err
		}
		composed = append(composed, GrantSource{Workspace: child.Name, Principals: principals})
	}
	own, err := ReadGrants(workspace.Dir(), environment)
	if err != nil {
		return nil, err
	}
	return MergeGrants(GrantSource{Workspace: workspace.Name, Principals: own}, composed)
}

// StatesGrants reports whether any workspace of the composition writes
// configurations/<env>/module-envelope.json at all. Reading the grants cannot
// tell: no file reads as an empty envelope, and "states nothing" is not the
// statement "grants nothing" ({"principals": {}}) when an envelope was
// delivered before.
func StatesGrants(workspace *resources.Workspace, environment string) (bool, error) {
	for _, dir := range append(composedDirsOf(workspace), workspace.Dir()) {
		_, err := os.Stat(grantsPath(dir, environment))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// StatesPrincipals reports whether any workspace of the composition states a
// "principals" OBJECT in configurations/<env>/module-envelope.json. The file
// existing is not that statement: a file written for another key — a
// minimum_revision floor, say — states no grants at all, and delivering its
// absence as {} revokes every grant on the cell under release custody, which
// stops every module principal minting. Core refuses a nil principals for
// exactly this reason ("an envelope granting nothing writes {}"), and the
// refusal has to be made here too, because by the time the grants are merged a
// renderer's omission and an authored {} are the same empty map. An explicit
// null is an omission, not a revocation.
func StatesPrincipals(workspace *resources.Workspace, environment string) (bool, error) {
	for _, dir := range append(composedDirsOf(workspace), workspace.Dir()) {
		stated, err := statesPrincipalsIn(dir, environment)
		if err != nil {
			return false, err
		}
		if stated {
			return true, nil
		}
	}
	return false, nil
}

func statesPrincipalsIn(workspaceDir, environment string) (bool, error) {
	path := grantsPath(workspaceDir, environment)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	raw, present := document["principals"]
	if !present {
		return false, nil
	}
	var principals map[string]json.RawMessage
	if err := json.Unmarshal(raw, &principals); err != nil || principals == nil {
		return false, nil
	}
	return true, nil
}

func composedDirsOf(workspace *resources.Workspace) []string {
	children := composedWorkspacesOf(workspace)
	dirs := make([]string, 0, len(children)+1)
	for _, child := range children {
		dirs = append(dirs, child.Dir())
	}
	return dirs
}

func grantsPath(workspaceDir, environment string) string {
	return filepath.Join(workspaceDir, "configurations", environment, "module-envelope.json")
}

// composedWorkspacesOf is every workspace the composition inherits from, the
// deepest first, so a base's base is read before the base.
func composedWorkspacesOf(workspace *resources.Workspace) []*resources.Workspace {
	children := workspace.ComposedWorkspaces()
	out := make([]*resources.Workspace, 0, len(children))
	for _, child := range children {
		out = append(out, composedWorkspacesOf(child)...)
		out = append(out, child)
	}
	return out
}

// MergeGrants is the composition's envelope policy from its sources: every
// composed workspace's principals, in order, then the consuming workspace's
// own, which decide. A principal two composed workspaces grant differently,
// and the consuming workspace does not decide, is refused by name — a quiet
// pick would hand a module an authority nobody agreed on.
func MergeGrants(own GrantSource, composed []GrantSource) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	owners := map[string]string{}
	for _, source := range composed {
		for prefix, grant := range source.Principals {
			if _, decided := own.Principals[prefix]; decided {
				continue
			}
			if previous, ok := out[prefix]; ok {
				if !sameGrant(previous, grant) {
					return nil, fmt.Errorf("module principal %q is granted differently by workspaces %q and %q, and workspace %q does not decide it: state its grant in %s/configurations/%s/module-envelope.json",
						prefix, owners[prefix], source.Workspace, own.Workspace, own.Workspace, "<environment>")
				}
				continue
			}
			out[prefix], owners[prefix] = grant, source.Workspace
		}
	}
	for prefix, grant := range own.Principals {
		out[prefix] = grant
	}
	return out, nil
}

// sameGrant compares two grants as documents, not as bytes: key order and
// whitespace are a file's, not a policy's.
func sameGrant(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
