package deployment

import (
	"fmt"
	"slices"
)

type profiles struct {
	execution ExecutionProfile
	identity  IdentityProfile
	trust     TrustProfile
	compiler  CompilerProfile
}

func loadProfiles(refs PlatformRefs, ctx Context) (profiles, error) {
	var out profiles
	if ctx.Schema != "codefly/validation-context/v1" {
		return out, refuse("CONTEXT_VERSION", "$/schema", "unsupported validation context")
	}
	docs := map[string]Document{}
	for _, d := range ctx.Documents {
		key := d.ID + "\x00" + d.Revision
		if !idPattern.MatchString(d.ID) || !idPattern.MatchString(d.Revision) || docs[key].ID != "" {
			return out, refuse("CONTEXT_DOCUMENT", "$/documents", "invalid or duplicate document identity")
		}
		docs[key] = d
	}
	references := []Reference{refs.Execution, refs.Identity, refs.Trust, refs.Compiler}
	seen := map[string]bool{}
	for i, r := range references {
		key := r.ID + "\x00" + r.Revision
		if seen[key] {
			return out, refuse("PROFILE_REFERENCE", "$/platform_refs", "profile references must be distinct")
		}
		seen[key] = true
		d, ok := docs[key]
		if !ok {
			return out, refuse("PROFILE_MISSING", "$/platform_refs", "referenced profile missing from independent context")
		}
		if !digestPattern.MatchString(r.Digest) || Digest(d.Content) != r.Digest {
			return out, refuse("PROFILE_DIGEST", "$/platform_refs", "retained profile bytes do not match reference")
		}
		var err error
		switch i {
		case 0:
			out.execution, err = decode[ExecutionProfile](d.Content)
		case 1:
			out.identity, err = decode[IdentityProfile](d.Content)
		case 2:
			out.trust, err = decode[TrustProfile](d.Content)
		case 3:
			out.compiler, err = decode[CompilerProfile](d.Content)
		}
		if err != nil {
			return out, err
		}
	}
	if len(docs) != len(references) {
		return out, refuse("CONTEXT_DOCUMENT", "$/documents", "unreferenced profile documents refused")
	}
	if out.execution.Schema != "codefly/execution-profile/v1" || out.identity.Schema != "codefly/identity-profile/v1" || out.trust.Schema != "codefly/trust-profile/v1" || out.compiler.Schema != ProfileVersion {
		return out, refuse("PROFILE_VERSION", "$/documents", "unsupported profile schema")
	}
	if out.compiler.KubernetesVersion != KubernetesVersion || out.compiler.Normalization != "explicit-v1" || len(out.compiler.ExcludedFields) != 0 {
		return out, refuse("COMPILER_PROFILE", "$/documents", "requires Kubernetes 1.34 explicit-v1, with no excluded fields")
	}
	if len(out.compiler.SupportedKinds) == 0 || !unique(out.compiler.SupportedKinds) {
		return out, refuse("COMPILER_PROFILE", "$/documents", "supported kinds must be a nonempty unique subset")
	}
	for _, kind := range out.compiler.SupportedKinds {
		if !slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}, kind) {
			return out, refuse("COMPILER_PROFILE", "$/documents", "unsupported controller kind in compiler profile")
		}
	}
	if len(out.execution.Platforms) == 0 || len(out.execution.CredentialKinds) == 0 || !unique(out.execution.CredentialKinds) {
		return out, refuse("EXECUTION_PROFILE", "$/documents", "platforms and credential kinds must be explicit and nonempty")
	}
	platforms := map[Platform]bool{}
	for _, p := range out.execution.Platforms {
		if p.OS != "linux" || !slices.Contains([]string{"amd64", "arm64"}, p.Architecture) || p.Variant != "" || platforms[p] {
			return out, refuse("EXECUTION_PROFILE", "$/documents", "unsupported or duplicate execution platform")
		}
		platforms[p] = true
	}
	for _, kind := range out.execution.CredentialKinds {
		if !slices.Contains([]string{"none", "application", "delivery", "platform"}, kind) {
			return out, refuse("EXECUTION_PROFILE", "$/documents", "unsupported credential kind")
		}
	}
	ids := map[string]bool{}
	for _, s := range out.execution.Sources {
		if !idPattern.MatchString(s.ID) || s.Location == "" || ids[s.ID] {
			return out, refuse("CATALOG_ENTRY", "$/documents", "invalid or duplicate source")
		}
		ids[s.ID] = true
	}
	ids = map[string]bool{}
	for _, e := range out.execution.Catalog {
		if !idPattern.MatchString(e.ID) || ids[e.ID] || !slices.Contains([]string{"configmap", "secret", "endpoint", "resource", "fact", "grant", "attachment", "role", "carrier"}, e.Kind) || !digestPattern.MatchString(e.Digest) || e.Version == "" || e.Name == "" || !unique(e.Workloads) || !unique(e.CredentialKinds) {
			return out, refuse("CATALOG_ENTRY", "$/documents", "invalid or duplicate catalogue entry")
		}
		ids[e.ID] = true
	}
	grants := map[string]bool{}
	for _, g := range out.identity.Grants {
		if grants[g.Workload] || !idPattern.MatchString(g.Workload) {
			return out, refuse("IDENTITY_GRANT", "$/documents", "duplicate or invalid identity grant")
		}
		grants[g.Workload] = true
	}
	verifierIDs := map[string]bool{}
	if len(out.trust.Verifiers) == 0 {
		return out, refuse("TRUST_PROFILE", "$/documents", "verifier references must not be empty")
	}
	for _, r := range out.trust.Verifiers {
		key := r.ID + "\x00" + r.Revision
		if !idPattern.MatchString(r.ID) || !idPattern.MatchString(r.Revision) || !digestPattern.MatchString(r.Digest) || verifierIDs[key] {
			return out, refuse("TRUST_PROFILE", "$/documents", "invalid or duplicate verifier reference")
		}
		verifierIDs[key] = true
	}
	for _, digest := range sortedKeys(ctx.Blobs) {
		if !digestPattern.MatchString(digest) || Digest(ctx.Blobs[digest]) != digest {
			return out, refuse("BLOB_DIGEST", "$/blobs/"+digest, "retained blob digest mismatch")
		}
	}
	return out, nil
}
func unique(xs []string) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		if x == "" || seen[x] {
			return false
		}
		seen[x] = true
	}
	return true
}
func referenceError(path, detail string) error {
	return refuse("REFERENCE_RESOLUTION", path, fmt.Sprintf("unresolved or mismatched %s", detail))
}
