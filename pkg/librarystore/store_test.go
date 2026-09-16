package librarystore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreviewIdentity(t *testing.T) {
	cfg := StoreConfig{GoOwner: "codefly-dev", PythonOwner: "codefly-dev", NpmRegistry: "https://npm.pkg.github.com", NpmScope: "@codefly-dev"}

	importPath, hint, err := PreviewIdentity(LanguageGo, cfg, RepositoryPolicy{}, "authkit", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "github.com/codefly-dev/authkit-go", importPath)
	require.Equal(t, "go get github.com/codefly-dev/authkit-go@v1.0.0", hint)

	importPath, hint, err = PreviewIdentity(LanguagePython, cfg, RepositoryPolicy{}, "authkit", "v1.0.0")
	require.NoError(t, err)
	require.Equal(t, "github.com/codefly-dev/authkit-python", importPath)
	require.Equal(t, `pip install "git+https://github.com/codefly-dev/authkit-python@v1.0.0"`, hint)

	importPath, hint, err = PreviewIdentity(LanguageTypeScript, cfg, RepositoryPolicy{}, "authkit", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "@codefly-dev/authkit", importPath)
	require.Equal(t, "npm install @codefly-dev/authkit@1.0.0", hint)
}

func TestPreviewIdentityRequiresConfiguration(t *testing.T) {
	_, _, err := PreviewIdentity(LanguageGo, StoreConfig{}, RepositoryPolicy{}, "authkit", "1.0.0")
	require.ErrorContains(t, err, "go.owner")

	_, _, err = PreviewIdentity(LanguageTypeScript, StoreConfig{}, RepositoryPolicy{}, "authkit", "1.0.0")
	require.ErrorContains(t, err, "typescript")

	_, _, err = PreviewIdentity(LanguageGo, StoreConfig{GoOwner: "codefly-dev"}, RepositoryPolicy{}, "authkit", "latest")
	require.ErrorContains(t, err, "semantic version")
}

// TestPipGitArgumentMatchesPythonInstallHint proves the argument a caller
// invoking `pip install` directly (codefly install library --destination)
// would pass is exactly the URL@tag pythonInstallHint documents in
// InstallHint — the same value, not two independently derived copies of it.
func TestPipGitArgumentMatchesPythonInstallHint(t *testing.T) {
	arg := PipGitArgument("github.com/codefly-dev/authkit-python", "1.0.0")
	require.Equal(t, "git+https://github.com/codefly-dev/authkit-python@v1.0.0", arg)
	require.Equal(t, `pip install "`+arg+`"`, pythonInstallHint("github.com/codefly-dev/authkit-python", "1.0.0", visibilityUnknown))
}

func TestNewStoreForRequiresConfiguration(t *testing.T) {
	_, err := NewStoreFor(LanguageGo, StoreConfig{}, RepositoryPolicy{})
	require.Error(t, err)

	store, err := NewStoreFor(LanguageGo, StoreConfig{GoOwner: "codefly-dev"}, RepositoryPolicy{})
	require.NoError(t, err)
	require.IsType(t, &GitHubStore{}, store)

	store, err = NewStoreFor(LanguageTypeScript, StoreConfig{NpmRegistry: "https://npm.pkg.github.com", NpmScope: "@codefly-dev"}, RepositoryPolicy{})
	require.NoError(t, err)
	require.IsType(t, &NpmStore{}, store)

	_, err = NewStoreFor(Language("rust"), StoreConfig{}, RepositoryPolicy{})
	require.ErrorContains(t, err, "unsupported language")
}
