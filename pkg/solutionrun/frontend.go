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
// and the CLI delivers it in presence. Core owns the strict reader for both.
const FrontendFileName = "solution.frontend.codefly.yaml"

// DeclaredFrontend reads either component kind's optional frontend beside its
// entry service. No entry or no file means a headless component. A present file
// is parsed with Core's presence rules; malformed declarations fail delivery.
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

// parseFrontend hands the declared file to core's own reader: the same strict
// decoder and frontend rules that admit the block on a presence, so accepting
// the file here cannot disagree with signing it. An empty file is invalid to
// core; an absent one is a headless component for either kind.
func parseFrontend(raw []byte, component string) (*solutionhost.Frontend, error) {
	frontend, err := solutionhost.ParseFrontend(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", component, FrontendFileName, err)
	}
	return frontend, nil
}
