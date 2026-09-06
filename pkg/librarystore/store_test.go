package librarystore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreviewIdentity(t *testing.T) {
	cfg := StoreConfig{GoOwner: "codefly-dev", PythonOwner: "codefly-dev", NpmRegistry: "https://npm.pkg.github.com", NpmScope: "@codefly-dev"}

	importPath, hint, err := PreviewIdentity(LanguageGo, cfg, "authkit", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "github.com/codefly-dev/authkit-go", importPath)
	require.Equal(t, "go get github.com/codefly-dev/authkit-go@v1.0.0", hint)

	importPath, hint, err = PreviewIdentity(LanguagePython, cfg, "authkit", "v1.0.0")
	require.NoError(t, err)
	require.Equal(t, "github.com/codefly-dev/authkit-python", importPath)
	require.Equal(t, `pip install "git+https://github.com/codefly-dev/authkit-python@v1.0.0"`, hint)

	importPath, hint, err = PreviewIdentity(LanguageTypeScript, cfg, "authkit", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "@codefly-dev/authkit", importPath)
	require.Equal(t, "npm install @codefly-dev/authkit@1.0.0", hint)
}

func TestPreviewIdentityRequiresConfiguration(t *testing.T) {
	_, _, err := PreviewIdentity(LanguageGo, StoreConfig{}, "authkit", "1.0.0")
	require.ErrorContains(t, err, "go.owner")

	_, _, err = PreviewIdentity(LanguageTypeScript, StoreConfig{}, "authkit", "1.0.0")
	require.ErrorContains(t, err, "typescript")

	_, _, err = PreviewIdentity(LanguageGo, StoreConfig{GoOwner: "codefly-dev"}, "authkit", "latest")
	require.ErrorContains(t, err, "semantic version")
}

func TestNewStoreForRequiresConfiguration(t *testing.T) {
	_, err := NewStoreFor(LanguageGo, StoreConfig{})
	require.Error(t, err)

	store, err := NewStoreFor(LanguageGo, StoreConfig{GoOwner: "codefly-dev"})
	require.NoError(t, err)
	require.IsType(t, &GitHubStore{}, store)

	store, err = NewStoreFor(LanguageTypeScript, StoreConfig{NpmRegistry: "https://npm.pkg.github.com", NpmScope: "@codefly-dev"})
	require.NoError(t, err)
	require.IsType(t, &NpmStore{}, store)

	_, err = NewStoreFor(Language("rust"), StoreConfig{})
	require.ErrorContains(t, err, "unsupported language")
}
