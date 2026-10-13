package solutionrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// FrontendFileName is the one file a solution declares its frontend in,
// beside its backend service's service.codefly.yaml. The solution runtime
// embeds it and serves its manifest route from it; the CLI reads the same file
// into the presence document's frontend block (core's
// solutionhost.Frontend), so the host lists the solution under what the
// solution itself serves. There is no other source: a running solution
// reports nothing to the host, and a presence document carries what the
// composition declared.
//
// The file is YAML, its top-level mapping is the block itself, and its keys
// are core's field names, field for field. It is decoded strictly: an unknown
// key is refused by name, because the runtime refuses it at boot and a key
// the CLI dropped silently would be a page the host lists under something
// the solution never declared.
const FrontendFileName = "solution.frontend.codefly.yaml"

// defaultExposedModule is the Module Federation module a solution exposes as
// its page when its declaration names none — the runtime's default, applied
// here too so the delivered block is what the solution serves.
const defaultExposedModule = "./Page"

// DeclaredFrontend reads the frontend block a composed solution declares,
// from the file beside its entry service — in the directory the module
// resolves that service to (Module.ServicePath), the one its
// service.codefly.yaml is read from. A solution that declares none is refused
// by name: the host activates a solution on a presence that carries the
// block, so a solution without one is a solution with no page, and the place
// to say so is the render, not a registry entry that never appears.
// Validation of the block's content is core's, at the presence document's.
func DeclaredFrontend(ctx context.Context, module *resources.Module) (*solutionhost.Frontend, error) {
	if module == nil || module.ServiceEntry == "" {
		return nil, fmt.Errorf("solution %s declares no service-entry, so it has no backend service to declare %s beside", module.Name, FrontendFileName)
	}
	if module.Dir() == "" {
		return nil, fmt.Errorf("solution %s has no directory, so %s cannot be read beside its service-entry", module.Name, FrontendFileName)
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
		return nil, fmt.Errorf("solution %s declares no frontend: write %s beside %s/%s — the one source of the title, order, exposed module, host contract, dashboard and surfaces the host lists the solution under, which its runtime embeds and this render delivers in its presence", module.Name, FrontendFileName, module.ServiceEntry, resources.ServiceConfigurationName)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseFrontend(raw, module.Name)
}

// parseFrontend decodes one declared file into core's block. yaml.v3 keeps a
// dashboard's whole numbers as integers, which core's canonical encoding
// requires; encoding/json would read them as float64.
func parseFrontend(raw []byte, solution string) (*solutionhost.Frontend, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("solution %s: %s is empty; it declares the frontend the host lists the solution under", solution, FrontendFileName)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var declared solutionhost.Frontend
	if err := decoder.Decode(&declared); err != nil {
		return nil, fmt.Errorf("solution %s: %s cannot be decoded: %w", solution, FrontendFileName, err)
	}
	// One document: a second one would be dropped silently otherwise, and a
	// file that looks like two declarations is not one somebody meant.
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("solution %s: %s carries more than one YAML document; it declares one frontend", solution, FrontendFileName)
	}
	if declared.ExposedModule == "" {
		declared.ExposedModule = defaultExposedModule
	}
	return &declared, nil
}
