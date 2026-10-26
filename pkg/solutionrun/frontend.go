package solutionrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// FrontendFileName is the optional frontend declaration for either component
// kind, beside its entry service's service.codefly.yaml. The runtime embeds it
// and the CLI delivers it in the module manifest. Core owns the strict reader for both.
const FrontendFileName = "solution.frontend.codefly.yaml"

// DeclaredFrontend reads either component kind's optional frontend beside its
// entry service. No entry or no file means a headless component. A present file
// is parsed with Core's manifest rules; malformed declarations fail delivery.
func DeclaredFrontend(ctx context.Context, module *resources.Module) (*solutionhost.Frontend, error) {
	if module == nil {
		return nil, fmt.Errorf("cannot read %s without a component", FrontendFileName)
	}
	if module.ServiceEntry == "" {
		return nil, nil
	}
	if module.Dir() == "" {
		return nil, fmt.Errorf("component %s has no directory, so %s cannot be read beside its service-entry", module.Name, FrontendFileName)
	}
	entryDir := filepath.Join(module.ServicesDir(), module.ServiceEntry)
	for _, reference := range module.ServiceReferences {
		if reference != nil && reference.Name == module.ServiceEntry {
			entryDir = module.ServicePath(ctx, reference)
		}
	}
	path := filepath.Join(entryDir, FrontendFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseFrontend(raw, module.Name)
}

// FrontendBundleDir is where a component's frontend build writes its complete
// bundle, under the module's services directory beside the entry service:
// the module-federation remote's output, whose root holds its manifest
// (FrontendBundleManifest) and the complete dist. The root of the bundle's
// canonical archive is this directory.
const FrontendBundleDir = "fe-remote/dist"

// FrontendBundleManifest is the file at a bundle's root that names what the
// remote exposes; a dist without it loads nothing and is not a bundle.
const FrontendBundleManifest = "mf-manifest.json"

// FrontendBundle is the built bundle of a component, when its frontend build
// wrote one: the directory FrontendBundleDir with FrontendBundleManifest at
// its root. No such directory is no bundle (a generation whose bundle is not
// built, which a manifest carries as a frontend without a payload); a
// directory without the manifest at its root is refused by name, since a host
// would otherwise admit bytes that load nothing.
func FrontendBundle(module *resources.Module) (string, bool, error) {
	if module == nil || module.Dir() == "" {
		return "", false, nil
	}
	root := filepath.Join(module.ServicesDir(), filepath.FromSlash(FrontendBundleDir))
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the frontend bundle of %s: %w", module.Name, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("%s: %s is not a directory, so it is not a frontend bundle", module.Name, root)
	}
	if _, err = os.Stat(filepath.Join(root, FrontendBundleManifest)); err != nil {
		return "", false, fmt.Errorf("%s: %s holds no %s at its root, so it is not a frontend bundle", module.Name, root, FrontendBundleManifest)
	}
	return root, true, nil
}

// parseFrontend hands the declared file to core's own reader: the same strict
// decoder and frontend rules that admit the block on a manifest, so accepting
// the file here cannot disagree with signing it. An empty file is invalid to
// core; an absent one is a headless component for either kind.
func parseFrontend(raw []byte, component string) (*solutionhost.Frontend, error) {
	frontend, err := solutionhost.ParseFrontend(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", component, FrontendFileName, err)
	}
	return frontend, nil
}
