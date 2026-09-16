package librarystore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadStoreConfig(t *testing.T) {
	dir := t.TempDir()
	yaml := `
name: platform
layout: flat
libraries:
  publish:
    go:
      owner: codefly-dev
    typescript:
      registry: https://npm.pkg.github.com
      scope: "@codefly-dev"
    python:
      owner: codefly-dev
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(yaml), 0o644))

	cfg, err := LoadStoreConfig(dir)
	require.NoError(t, err)
	require.Equal(t, StoreConfig{
		GoOwner:     "codefly-dev",
		NpmRegistry: "https://npm.pkg.github.com",
		NpmScope:    "@codefly-dev",
		PythonOwner: "codefly-dev",
	}, cfg)
}

func TestLoadStoreConfigWithoutLibrariesBlockIsEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: platform\nlayout: flat\n"), 0o644))

	cfg, err := LoadStoreConfig(dir)
	require.NoError(t, err)
	require.Equal(t, StoreConfig{}, cfg)
}

func TestLoadStoreConfigMissingWorkspaceFile(t *testing.T) {
	_, err := LoadStoreConfig(t.TempDir())
	require.Error(t, err)
}

// The repository-creation policy is a command-line decision, never a file one.
// A workspace configuration travels with the module it describes, so letting it
// carry these fields would let a checked-in file authorize creating — and, with
// the public flag, disclosing — a repository on whoever ran the publish.
func TestLoadStoreConfigNeverCarriesTheRepositoryCreationPolicy(t *testing.T) {
	dir := t.TempDir()
	yaml := `
name: platform
layout: flat
libraries:
  publish:
    createMissingRepositories: true
    publicRepositories: true
    go:
      owner: codefly-dev
      createMissingRepositories: true
      publicRepositories: true
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(yaml), 0o644))

	cfg, err := LoadStoreConfig(dir)
	require.NoError(t, err)
	require.Equal(t, "codefly-dev", cfg.GoOwner, "the owner is a file-level fact and still loads")
	require.False(t, cfg.CreateMissingRepositories, "a workspace file must not be able to authorize repository creation")
	require.False(t, cfg.PublicRepositories, "a workspace file must not be able to make a repository public")

	// And the store built from it creates nothing.
	store, err := NewStoreFor(LanguageGo, cfg)
	require.NoError(t, err)
	require.Nil(t, store.(*GitHubStore).ensureRepository)
}
