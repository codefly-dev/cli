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
