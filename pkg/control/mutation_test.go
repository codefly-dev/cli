package control

import (
	"context"
	"testing"
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
	if err := p.ApplyPreparedMutation(ctx, token); err != nil {
		t.Fatal(err)
	}
	data, _ := p.ReadFile(ctx, fixtureFile)
	if string(data) != "bye" {
		t.Errorf("after prepared edit = %q, want bye", data)
	}

	// The token is single-use — a second apply must fail.
	if err := p.ApplyPreparedMutation(ctx, token); err == nil {
		t.Error("prepared mutation token should be single-use")
	}
}

func TestApplyUnknownTokenFails(t *testing.T) {
	if err := New().ApplyPreparedMutation(context.Background(), PreparedMutation{Token: "deadbeef"}); err == nil {
		t.Error("applying an unknown token should fail")
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
