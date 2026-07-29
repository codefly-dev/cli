package control

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
)

func TestConfigureMutationAuthorityRejectsUnknownMode(t *testing.T) {
	if err := New().ConfigureMutationAuthority(context.Background(), AuthorityConfig{Mode: "bogus"}); err == nil {
		t.Error("expected unknown authority mode to be rejected")
	}
}

func TestPrepareMutationValidatesPayload(t *testing.T) {
	ctx := context.Background()
	p := New()
	if _, err := p.PrepareMutation(ctx, Mutation{Kind: MutationFile, Payload: "not an edit"}); err == nil {
		t.Error("file mutation with non-Edit payload should fail at prepare")
	}
	if _, err := p.PrepareMutation(ctx, Mutation{Kind: MutationDeploy, Payload: 42}); err == nil {
		t.Error("deploy mutation with non-DeployRequest payload should fail at prepare")
	}
	if _, err := p.PrepareMutation(ctx, Mutation{Kind: MutationGitOpsPublish, Payload: 42}); err == nil {
		t.Error("gitops publish mutation with invalid payload should fail at prepare")
	}
}

func TestPreparedGitOpsPublicationDispatchesAndConsumesAuthority(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	p := New()
	if err := p.ConfigureMutationAuthority(ctx, AuthorityConfig{Mode: AuthorityPrepared}); err != nil {
		t.Fatal(err)
	}
	token, err := p.PrepareMutation(ctx, Mutation{
		Kind: MutationGitOpsPublish,
		Payload: gitops.PublishMutation{
			Request: gitops.PublishRequest{Module: "backend", Environment: "production"},
			PlanID:  "sha256:inspected",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApplyPreparedMutation(ctx, token); err == nil {
		t.Fatal("publication without workspace.gitops unexpectedly succeeded")
	}
	if _, err := p.ApplyPreparedMutation(ctx, token); err == nil {
		t.Fatal("failed publication authority was not consumed")
	}
}

func TestPreparedFileMutationAppliesOnceThenIsConsumed(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	p := New()

	if err := p.WriteFile(ctx, fixtureFile, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	token, err := p.PrepareMutation(ctx, Mutation{
		Kind:    MutationFile,
		Payload: Edit{Path: fixtureFile, OldText: "hi", NewText: "bye"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// First apply performs the edit.
	if _, err := p.ApplyPreparedMutation(ctx, token); err != nil {
		t.Fatal(err)
	}
	data, _ := p.ReadFile(ctx, fixtureFile)
	if string(data) != "bye" {
		t.Errorf("after prepared edit = %q, want bye", data)
	}

	// The token is single-use — a second apply must fail.
	if _, err := p.ApplyPreparedMutation(ctx, token); err == nil {
		t.Error("prepared mutation token should be single-use")
	}
}

func TestApplyUnknownTokenFails(t *testing.T) {
	if _, err := New().ApplyPreparedMutation(context.Background(), PreparedMutation{Token: "deadbeef"}); err == nil {
		t.Error("applying an unknown token should fail")
	}
}

func TestExecuteDeployMutationReturnsDeploymentEvidence(t *testing.T) {
	want := DeployResult{
		Succeeded: true,
		RenderedTrees: []RenderedTree{{
			Module:    "backend",
			Service:   "api",
			Digest:    "sha256:rendered",
			Manifests: "kind: Deployment\n",
		}},
	}
	executor := mutationExecutorStub{deployResult: want}

	result, err := executeMutation(context.Background(), executor, Mutation{
		Kind:    MutationDeploy,
		Payload: DeployRequest{Service: "backend/api"},
	}, mutationauthority.NewPreparedPermit())

	if err != nil {
		t.Fatal(err)
	}
	if result.Deploy == nil {
		t.Fatal("prepared deploy returned no deployment result")
	}
	if result.Deploy.RenderedTrees[0].Digest != want.RenderedTrees[0].Digest {
		t.Fatalf("prepared deploy digest = %q, want %q", result.Deploy.RenderedTrees[0].Digest, want.RenderedTrees[0].Digest)
	}
}

func TestDeployRefusedUnderPreparedAuthority(t *testing.T) {
	ctx := context.Background()
	p := New()
	if err := p.ConfigureMutationAuthority(ctx, AuthorityConfig{Mode: AuthorityPrepared}); err != nil {
		t.Fatal(err)
	}
	// A direct Deploy must be refused before any flow work under prepared
	// authority — this resolves before loading a workspace, so it's hermetic.
	if _, err := p.Deploy(ctx, DeployRequest{Service: "backend/api"}); err == nil {
		t.Error("direct Deploy should be refused under prepared authority")
	}
}

type mutationExecutorStub struct {
	deployResult DeployResult
}

func (mutationExecutorStub) ApplyEdit(context.Context, Edit) error {
	return nil
}

func (s mutationExecutorStub) runDeploy(context.Context, DeployRequest) (DeployResult, error) {
	return s.deployResult, nil
}

func (mutationExecutorStub) publishGitOps(context.Context, gitops.PublishMutation, mutationauthority.PreparedPermit) (gitops.PublishResult, error) {
	return gitops.PublishResult{}, nil
}

func (mutationExecutorStub) rollbackGitOps(context.Context, gitops.RollbackMutation, mutationauthority.PreparedPermit) (gitops.PublishResult, error) {
	return gitops.PublishResult{}, nil
}
