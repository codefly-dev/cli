package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"

	"github.com/codefly-dev/cli/pkg/environments"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// PreparedPairedDocument is the document a render writes and `codefly delivery
// apply` sends: one ApplyManifestRequest carrying both signed halves and NO
// credential.
//
// The two halves are settled and signed where release custody is — at publish
// — and sent from inside the cell, after the module's workloads exist. The
// credential is the one field that cannot travel between those two moments: a
// delivery credential is a projected ServiceAccount token with a lifetime of
// minutes, so one written here would be a published secret and would be
// expired by the time the manifest synced. It is left empty deliberately, and
// the sender refuses a document that holds one rather than sending it.
//
// carrier is the Core signed carrier exactly as it is delivered
// (solutionhost.MarshalSigned of the settled manifest), and declaration is the
// captured AuthorityManifestV1 bytes — nil for a withdrawal, which keeps its
// Core-only identity. The authority half is bound to the carrier's own
// document bytes by SignedAuthorityDocument, so the pair the host receives is
// the pair that was signed.
func PreparedPairedDocument(carrier []byte, declaration []byte, signer solutionhost.Signer) ([]byte, error) {
	if len(carrier) == 0 {
		return nil, errors.New("a prepared delivery document needs the signed Core carrier")
	}
	signed, err := solutionhost.ParseSigned(carrier)
	if err != nil {
		return nil, fmt.Errorf("read the signed Core carrier: %w", err)
	}
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	if err != nil {
		return nil, fmt.Errorf("read the module manifest from the signed carrier: %w", err)
	}
	request := &compositionv1.ApplyManifestRequest{
		Carrier: &compositionv1.SignedManifestV1{Document: signed.Document, Envelope: signed.Envelope},
	}
	if request.Authority, err = SignedAuthorityDocument(manifest, signed.Document, declaration, signer); err != nil {
		return nil, fmt.Errorf("sign the authority half of %s: %w", manifest.Binding, err)
	}
	// A prepared document is rendered into a manifest, so a re-render of
	// unchanged content must write identical bytes or every cell sees a change
	// that is only an encoding — and the Job's name carries the digest of what
	// it delivers, so an unstable encoding is a new Job every sync. protojson
	// deliberately perturbs its whitespace, and its seed is per-binary, so two
	// CLI builds can encode one message differently. Compacting the result
	// removes every insignificant byte, leaving protojson's field order, which
	// is the message's own.
	encoded, err := protojson.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode the prepared delivery document of %s: %w", manifest.Binding, err)
	}
	var stable bytes.Buffer
	if err = json.Compact(&stable, encoded); err != nil {
		return nil, fmt.Errorf("encode the prepared delivery document of %s: %w", manifest.Binding, err)
	}
	data := stable.Bytes()
	// Read it back through the sender's own reader, so a document this writer
	// produced that the sender would refuse is caught where it is written.
	if _, err = parsePreparedDocument(manifest.Binding, data); err != nil {
		return nil, fmt.Errorf("the prepared delivery document does not read back: %w", err)
	}
	return data, nil
}

// PreparePairedDocuments writes one prepared paired document per module
// manifest a render left in overlay, into out, and answers the file names in
// the order they were written.
//
// The carriers come from the overlay the render wrote — the signed Core halves
// already settled at their generations — and the declarations from the modules
// themselves through sources. So this prepares exactly what is about to be
// delivered: nothing is re-rendered, no generation is re-settled, and a
// manifest the overlay does not carry is not invented here.
//
// A manifest whose carrier is absent is refused by name rather than prepared
// Core-only: an unsigned manifest cannot carry a signed authority half beside
// it, and a document that quietly dropped the half would reach the host
// looking like a success and compose a module with no roles.
func PreparePairedDocuments(ctx context.Context, overlay, out string, sources AuthoritySources, signer solutionhost.Signer) ([]string, error) {
	rendered, err := renderedCarriers(overlay)
	if err != nil {
		return nil, err
	}
	if len(rendered) == 0 {
		return nil, fmt.Errorf("no module manifest is rendered under %s, so there is nothing to deliver", overlay)
	}
	if err = os.MkdirAll(out, 0o755); err != nil {
		return nil, fmt.Errorf("create the prepared delivery directory: %w", err)
	}
	bindings := make([]string, 0, len(rendered))
	for binding := range rendered {
		bindings = append(bindings, binding)
	}
	sort.Strings(bindings)
	written := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		entry := rendered[binding]
		if len(entry.carrier) == 0 {
			return nil, fmt.Errorf("the module manifest of %s carries no signature, so no authority half can be signed beside it; publish it signed before preparing a paired delivery", binding)
		}
		// A withdrawal declares nothing and keeps its Core-only identity.
		var declaration []byte
		if !entry.manifest.Removed {
			if declaration, err = sources.Declaration(ctx, entry.manifest.Alias); err != nil {
				return nil, fmt.Errorf("read the declaration of module %s: %w", entry.manifest.Alias, err)
			}
		}
		document, prepareErr := PreparedPairedDocument(entry.carrier, declaration, signer)
		if prepareErr != nil {
			return nil, prepareErr
		}
		name := binding + PreparedDeliveryDocument
		if err = os.WriteFile(filepath.Join(out, name), document, 0o600); err != nil {
			return nil, err
		}
		written = append(written, name)
	}
	return written, nil
}

// WorkspaceAuthoritySources are the five declarations read from a composed
// workspace, exactly as a native run reads them at the moment of a delivery
// (launcher.newCompositionDelivery). A publish preparing a cell's documents
// reads them the same way, so neither producer can read a different module
// than the other.
func WorkspaceAuthoritySources(workspace *resources.Workspace, env *environments.Environment) AuthoritySources {
	dir := func(ctx context.Context, module string) (string, error) {
		m, err := workspace.LoadModuleFromName(ctx, module)
		if err != nil {
			return "", err
		}
		return m.Dir(), nil
	}
	return AuthoritySources{
		Methods: func(ctx context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error) {
			m, err := workspace.LoadModuleFromName(ctx, module)
			if err != nil {
				return nil, false, err
			}
			return HostMethodPolicies(ctx, workspace, env, m)
		},
		Ceilings: func(ctx context.Context, module string) ([]*authorityv1.WorkScopeV1, error) {
			at, err := dir(ctx, module)
			if err != nil {
				return nil, err
			}
			return ScopeCeilings(at)
		},
		Roles: func(ctx context.Context, module string) ([]*compositionv1.RoleV1, error) {
			at, err := dir(ctx, module)
			if err != nil {
				return nil, err
			}
			return Roles(at)
		},
		OwnerAware: func(ctx context.Context, module string) (bool, error) {
			at, err := dir(ctx, module)
			if err != nil {
				return false, err
			}
			return OwnerAware(at)
		},
		Installation: func(ctx context.Context, module string) (string, []*compositionv1.EdgeDependencyV1, error) {
			at, err := dir(ctx, module)
			if err != nil {
				return "", nil, err
			}
			return InstallationDeclaration(at)
		},
	}
}

// renderedCarriers reads the module manifests a render wrote into overlay,
// keyed by binding, each WITH the signed carrier it holds.
//
// renderedManifests deliberately drops the carrier: at publish the carrier is
// settled fresh, and a stale one read back would be a signature nobody
// checked. Preparing a paired document is the opposite case — the carrier is
// the settled Core half and is exactly what must be sent — so it is read here
// rather than by widening that one.
func renderedCarriers(directory string) (map[string]deliveredManifest, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the rendered module manifests: %w", err)
	}
	rendered := map[string]deliveredManifest{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == kustomizationFile {
			continue
		}
		data, readErr := readWithin(directory, entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		manifest, carrier, held, parseErr := manifestFromConfigMap(data)
		if parseErr != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), parseErr)
		}
		if held {
			rendered[manifest.Binding] = deliveredManifest{manifest: manifest, carrier: carrier}
		}
	}
	return rendered, nil
}
