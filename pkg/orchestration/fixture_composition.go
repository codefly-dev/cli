package orchestration

import (
	"context"
	"maps"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/fixturefragments"
)

// mergedFixtureComposition is the one fixture-composition document of this
// composition: the merge of the fragment every composed module ships
// (fixturefragments, whose rules are the authority's).
//
// It is why this group is MULTI-PROVIDER, alone among workspace
// configurations. Core's single-provider rule is right for a group whose value
// one module owns — two modules defining it differently is ambiguity with no
// principled winner — and wrong for this one, where every composed module is
// *expected* to carry its own part and the parts combine. Since cli#1197 every
// module ships a fragment, so the moment a composition composes a second one
// core reports an ambiguity and the readiness check refuses a composition a
// run handles happily. Its suggested remedy — declare the group in the
// solution workspace — is also a refusal now: `codefly generate
// fixture-composition` will not overwrite a hand-written document, and the
// local authority reads the merge, not one module's part.
//
// Supplying the merge here resolves it where every path can see the same
// answer: this reader is the one loader run, CI, deployment and `codefly
// doctor workspace` share, so none of them can disagree about this group.
// Nothing is exempted from the single-provider rule — the group simply has a
// provider, this one, exactly as hop-routes does.
//
// A workspace that provides the group itself keeps it: a developer's
// `codefly generate fixture-composition` writes that file, a run writes it
// before it loads any configuration, and core's own precedence (the consuming
// workspace's declaration wins, and resolves an ambiguity between composed
// modules) then applies untouched. This supplies the merge only where the
// workspace has no answer of its own, which is the case in CI and in a render.
func mergedFixtureComposition(ctx context.Context, workspace *resources.Workspace, env *resources.Environment, configs []*basev0.Configuration) (*basev0.Configuration, error) {
	for _, info := range workspaceConfigurationInfos(configs) {
		if info.GetName() == fixturefragments.Group {
			// The workspace (or core's resolution of it) already answers.
			return nil, nil
		}
	}
	declared, err := fixturefragments.Declared(ctx, workspace)
	if err != nil || !declared {
		return nil, err
	}
	profiles, err := env.ConfigurationProfileNames()
	if err != nil {
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
