package orchestration

import (
	"context"
	"maps"
	"slices"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/fixturefragments"
)

// mergedFixtureComposition supplies the Authority-owned merge unless the
// workspace itself supplies the document. A module's resolved default is still
// a fragment, even when Core sees just one provider or identical providers.
func mergedFixtureComposition(ctx context.Context, workspace *resources.Workspace, env *resources.Environment, rootNames []string) (*basev0.Configuration, error) {
	if slices.Contains(rootNames, fixturefragments.Group) {
		return nil, nil
	}
	profiles, err := env.ConfigurationProfileNames()
	if err != nil {
		return nil, err
	}
	// Core overlays an explicit workspace document onto a single module's
	// default without classifying that group as a root-delivered group. Read
	// the root's profile through Core to preserve that explicit declaration too.
	own, err := configurations.LoadProfileConfigurations(ctx, workspace.Dir(), "configurations", profiles)
	if err != nil {
		return nil, err
	}
	for _, info := range own.Infos {
		if info.GetName() == fixturefragments.Group {
			return nil, nil
		}
	}
	declared, err := fixturefragments.Declared(ctx, workspace)
	if err != nil || !declared {
		return nil, err
	}
	document, err := fixturefragments.Merged(ctx, workspace, profiles)
	if err != nil || document == nil {
		return nil, err
	}
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: fixturefragments.Group,
		Data: &basev0.ConfigurationData{Kind: "yaml", Content: document},
	}}}, nil
}

// AmbiguousWorkspaceConfigurations is core's set minus every group this reader
// supplies itself. A group with a provider is not ambiguous, whoever the
// provider is: core reports fixture-composition ambiguous because two composed
// modules carry a fragment, and the merge of those fragments is this reader's
// answer to exactly that. Stated as a property of what was supplied rather
// than as a name, so a second generated group needs no second exemption.
func (r *WorkspaceConfigurationReader) AmbiguousWorkspaceConfigurations() map[string]error {
	ambiguous := r.ConfigurationInformationLocalReader.AmbiguousWorkspaceConfigurations()
	if len(ambiguous) == 0 {
		return ambiguous
	}
	supplied := map[string]bool{}
	for _, info := range workspaceConfigurationInfos(r.combined) {
		supplied[info.GetName()] = true
	}
	out := maps.Clone(ambiguous)
	for name := range ambiguous {
		if supplied[name] {
			delete(out, name)
		}
	}
	return out
}
