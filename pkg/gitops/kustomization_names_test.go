package gitops

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Every name this package will read a kustomization from has to be one
// kustomize itself recognises, or the tooling rewrites a file the build then
// ignores.
//
// kustomize's in-memory filesystem is case-sensitive on every platform, unlike
// macOS's on-disk one. That matters: the third recognised name is
// `Kustomization` with a capital K, and a lowercase copy of it satisfies an
// on-disk lookup on a case-insensitive filesystem while failing on Linux. This
// test therefore fails wherever it is run, instead of only in CI.
func TestEveryAcceptedKustomizationNameIsOneKustomizeRecognises(t *testing.T) {
	for _, name := range kustomizationFileNames {
		t.Run(name, func(t *testing.T) {
			fs := filesys.MakeFsInMemory()
			require.NoError(t, fs.MkdirAll("/overlay"))
			require.NoError(t, fs.WriteFile("/overlay/"+name,
				[]byte("apiVersion: "+kustomizeAPIVersion+"\nkind: "+kindKustomization+"\nresources: []\n")))
			_, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, "/overlay")
			require.NoError(t, err, "kustomize does not recognise %q as a kustomization", name)
		})
	}
	require.Equal(t, konfig.RecognizedKustomizationFileNames(), kustomizationFileNames,
		"the names this package reads must be kustomize's own, not a restatement that can drift")
	require.Equal(t, kustomizationFile, konfig.RecognizedKustomizationFileNames()[0])
	require.Equal(t, kustomizationFileAlt, konfig.RecognizedKustomizationFileNames()[1])
}
