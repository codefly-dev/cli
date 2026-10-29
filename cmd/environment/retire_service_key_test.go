package environment

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

// retireWorkspace writes a workspace that composes two services named
// object-storage, with `azure` carrying both the bare key and the qualified one
// the producer re-keyed to — the shape an import leaves behind, because it
// merges by key and removes nothing.
func retireWorkspace(t *testing.T, bareValues string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(resources.WorkspaceConfigurationName, `name: acme
layout: modules
modules:
    - name: documents
    - name: depot
environments:
    - name: azure
      namespace: acme
      service-config:
          services:
              object-storage:
`+bareValues+`
              documents/object-storage:
                  values:
                      BUCKET: shared
`)
	service := func(name string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\n" +
			"agent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n"
	}
	write(filepath.Join("modules", "documents", resources.ModuleConfigurationName), "kind: module\nname: documents\nservices:\n    - name: object-storage\n")
	write(filepath.Join("modules", "documents", "services", "object-storage", resources.ServiceConfigurationName), service("object-storage"))
	write(filepath.Join("modules", "depot", resources.ModuleConfigurationName), "kind: module\nname: depot\nservices:\n    - name: object-storage\n")
	write(filepath.Join("modules", "depot", "services", "object-storage", resources.ServiceConfigurationName), service("object-storage"))
	return dir
}

const retireContract = `{"schema":"codefly/coordinate/v1","coordinate":"hosted-eastus2","environment":{
  "name":"azure","namespace":"acme",
  "service-config":{"services":{"documents/object-storage":{"values":{"BUCKET":"shared"}}}}
}}`

func retireImport(t *testing.T, dir string, directives []string, dryRun bool) (string, error) {
	t.Helper()
	var out strings.Builder
	err := runImport(context.Background(), &importOptions{
		dir: dir, envName: "azure", contractData: []byte(retireContract),
		retireServiceKeys: directives, dryRun: dryRun, stdout: &out,
		now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	})
	return out.String(), err
}

const bareShared = `                  values:
                      BUCKET: shared`

// The stated retirement removes the bare key and keeps the qualified one.
func TestRetireRemovesTheStatedBareKey(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	out, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, false)
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !strings.Contains(out, "the whole entry is removed") {
		t.Errorf("stdout does not say the whole entry goes:\n%s", out)
	}
	env := loadWorkspace(t, dir).FindEnvironment("azure")
	if _, stale := env.ServiceConfig.Services["object-storage"]; stale {
		t.Error("the bare key survived an explicit retirement")
	}
	if _, ok := env.ServiceConfig.Services["documents/object-storage"]; !ok {
		t.Error("the destination was lost")
	}
}

// The WHOLE entry goes, including values the destination does not declare —
// ValuesFor overlays bare then qualified, so those keys stop being set. The
// run says which ones before writing.
func TestRetireNamesTheValueKeysItRemoves(t *testing.T) {
	dir := retireWorkspace(t, `                  values:
                      BUCKET: shared
                      LEGACY_REGION: eastus2`)
	out, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, false)
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !strings.Contains(out, "LEGACY_REGION") || !strings.Contains(out, "no longer set") {
		t.Errorf("stdout does not name the value key that is lost:\n%s", out)
	}
	env := loadWorkspace(t, dir).FindEnvironment("azure")
	if values := env.ServiceConfig.ValuesFor("documents", "object-storage"); values["LEGACY_REGION"] != "" {
		t.Errorf("LEGACY_REGION still resolves: %v", values)
	}
}

// --dry-run writes nothing, directives included.
func TestRetireDryRunWritesNothing(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	file := filepath.Join(dir, resources.WorkspaceConfigurationName)
	before := readFile(t, file)
	if _, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, true); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if readFile(t, file) != before {
		t.Error("--dry-run wrote the workspace")
	}
}

// Every refusal happens before the write: the file is byte-identical after.
func TestRetireRefusesBeforeWriting(t *testing.T) {
	for name, directive := range map[string]string{
		"malformed, no block":         "object-storage=documents/object-storage",
		"malformed, no destination":   "service-config:object-storage",
		"empty destination":           "service-config:object-storage=",
		"bare is qualified":           "service-config:saas/object-storage=documents/object-storage",
		"destination is bare":         "service-config:object-storage=object-storage",
		"unsupported block":           "service-secrets:object-storage=documents/object-storage",
		"unknown block":               "ingress:object-storage=documents/object-storage",
		"unknown destination":         "service-config:object-storage=nowhere/object-storage",
		"destination not in contract": "service-config:object-storage=depot/object-storage",
		"nothing to retire":           "service-config:absent=documents/object-storage",
	} {
		t.Run(name, func(t *testing.T) {
			dir := retireWorkspace(t, bareShared)
			file := filepath.Join(dir, resources.WorkspaceConfigurationName)
			before := readFile(t, file)
			if _, err := retireImport(t, dir, []string{directive}, false); err == nil {
				t.Fatal("expected a refusal")
			}
			if readFile(t, file) != before {
				t.Error("the workspace was modified by a refused import")
			}
		})
	}
}

// A legacy flat workspace must not be MIGRATED by the inspection a refused
// directive performs. Core's default loader saves workspace data and deletes
// the old module file on such a tree (postLoad), so the destination check runs
// under WithWorkspaceMigrationRefusal: a refusal leaves every file exactly as
// it was rather than half-migrating a workspace nobody asked to change.
func TestRetireRefusalDoesNotMigrateALegacyWorkspace(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A FLAT workspace is what reaches the migration branch: Core rewrites the
	// module list and deletes the legacy module file (postLoad).
	write(resources.WorkspaceConfigurationName, `name: acme
layout: flat
environments:
    - name: azure
      namespace: acme
      service-config:
          services:
              object-storage:
                  values:
                      BUCKET: shared
              documents/object-storage:
                  values:
                      BUCKET: shared
`)
	write(resources.ModuleConfigurationName, "kind: module\nname: acme\nservices:\n    - name: object-storage\n")
	write(filepath.Join("services", "object-storage", resources.ServiceConfigurationName),
		"kind: service\nname: object-storage\nversion: 0.0.0\n"+
			"agent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n")

	snapshot := func() map[string]string {
		t.Helper()
		files := map[string]string{}
		if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			files[path] = readFile(t, path)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := snapshot()

	if _, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, false); err == nil {
		t.Fatal("expected a refusal: the workspace has a legacy module pending migration")
	}

	after := snapshot()
	for path, content := range before {
		got, present := after[path]
		switch {
		case !present:
			t.Errorf("%s was deleted by a refused import", path)
		case got != content:
			t.Errorf("%s was rewritten by a refused import", path)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			t.Errorf("%s was created by a refused import", path)
		}
	}
}

// A bare key the INCOMING contract still declares is current, not retired:
// withdrawing it would delete what the very next import writes back.
func TestRetireRefusesAKeyTheContractStillDeclares(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	contract := `{"schema":"codefly/coordinate/v1","coordinate":"hosted-eastus2","environment":{
	  "name":"azure","namespace":"acme",
	  "service-config":{"services":{
	    "object-storage":{"values":{"BUCKET":"shared"}},
	    "documents/object-storage":{"values":{"BUCKET":"shared"}}}}
	}}`
	before := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	var out strings.Builder
	err := runImport(context.Background(), &importOptions{
		dir: dir, envName: "azure", contractData: []byte(contract),
		retireServiceKeys: []string{"service-config:object-storage=documents/object-storage"},
		stdout:            &out, now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "still declares") {
		t.Fatalf("expected a refusal naming the contract, got %v", err)
	}
	if readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName)) != before {
		t.Error("the workspace was modified by a refused import")
	}
}

// Duplicate and conflicting directives for one key are refused.
func TestRetireRefusesDuplicateDirectives(t *testing.T) {
	for name, directives := range map[string][]string{
		"same destination twice": {
			"service-config:object-storage=documents/object-storage",
			"service-config:object-storage=documents/object-storage",
		},
		"conflicting destinations": {
			"service-config:object-storage=documents/object-storage",
			"service-config:object-storage=depot/object-storage",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := retireWorkspace(t, bareShared)
			before := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
			if _, err := retireImport(t, dir, directives, false); err == nil || !strings.Contains(err.Error(), "twice") {
				t.Fatalf("expected a duplicate refusal, got %v", err)
			}
			if readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName)) != before {
				t.Error("the workspace was modified by a refused import")
			}
		})
	}
}

// Unrelated keys survive, whatever their values: retirement removes only what
// was named, and never compares values to decide.
func TestRetireLeavesUnrelatedKeysAlone(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	file := filepath.Join(dir, resources.WorkspaceConfigurationName)
	content := readFile(t, file)
	// depot/object-storage holds the SAME values as the destination, and
	// ledger holds different ones. Neither is named, so both must survive.
	content = strings.Replace(content, "              documents/object-storage:",
		"              depot/object-storage:\n                  values:\n                      BUCKET: shared\n"+
			"              ledger:\n                  values:\n                      MODE: local\n"+
			"              documents/object-storage:", 1)
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, false); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	services := loadWorkspace(t, dir).FindEnvironment("azure").ServiceConfig.Services
	for _, key := range []string{"depot/object-storage", "ledger", "documents/object-storage"} {
		if _, ok := services[key]; !ok {
			t.Errorf("unrelated key %q was removed: %+v", key, services)
		}
	}
	if _, stale := services["object-storage"]; stale {
		t.Error("the named key survived")
	}
}

// Without directives the importer behaves exactly as before: the bare key is
// preserved, which is the behaviour on main.
func TestImportWithoutDirectivesStillPreservesTheBareKey(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	if _, err := retireImport(t, dir, nil, false); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if _, ok := loadWorkspace(t, dir).FindEnvironment("azure").ServiceConfig.Services["object-storage"]; !ok {
		t.Error("an import with no directives removed the bare key")
	}
}

// The directive is one-shot: after a successful write the same command refuses,
// because the key it names is gone. A retry must omit the flag.
func TestRetireIsOneShot(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	directive := []string{"service-config:object-storage=documents/object-storage"}
	if _, err := retireImport(t, dir, directive, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, err := retireImport(t, dir, directive, false)
	if err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("expected a one-shot refusal naming the retry, got %v", err)
	}
	if _, err := retireImport(t, dir, nil, false); err != nil {
		t.Fatalf("retry without the flag must succeed: %v", err)
	}
}

// TestRetireFlagDoesNotLeakBetweenExecutions is the reviewer's public-surface
// finding: the flag was bound to a package-level slice, so a second
// Cmd.Execute in the SAME process still carried the first run's directives and
// failed the one-shot check against a key the first run had already retired.
// Separate processes were never affected, which is why the command tests
// missed it.
func TestRetireFlagDoesNotLeakBetweenExecutions(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	contract := filepath.Join(t.TempDir(), "contract.json")
	if err := os.WriteFile(contract, []byte(retireContract), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// The same package-level command object both times, which is what a single
	// process does.
	run := func(args ...string) error {
		Cmd.SetOut(io.Discard)
		Cmd.SetErr(io.Discard)
		Cmd.SetArgs(args)
		return Cmd.Execute()
	}
	base := []string{"import", "azure", "--coordinate-contract", contract}

	if err := run(append(base, "--retire-service-key", "service-config:object-storage=documents/object-storage")...); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	// Second execute WITHOUT the flag. It must not inherit the directive.
	if err := run(base...); err != nil {
		t.Fatalf("second execute without the flag inherited the first run's directive: %v", err)
	}
}

// TestRetireRefusesToAcquireAnUnmaterializedReference: the destination check
// reads the composition, and the host's resolver would clone an unmaterialized
// pinned reference into the module cache to answer. A check that acquires a
// dependency in order to decide it would refuse is not read-only, so the
// resolver is replaced for the inspection and the directive refuses instead.
func TestRetireRefusesToAcquireAnUnmaterializedReference(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(resources.WorkspaceConfigurationName, `name: acme
layout: modules
workspaces:
    - name: upstream
      source: obin-ai/not-materialized
      version: 0.1.0
modules:
    - name: documents
environments:
    - name: azure
      namespace: acme
      service-config:
          services:
              object-storage:
                  values:
                      BUCKET: shared
              documents/object-storage:
                  values:
                      BUCKET: shared
`)
	write(filepath.Join("modules", "documents", resources.ModuleConfigurationName), "kind: module\nname: documents\nservices:\n    - name: object-storage\n")
	write(filepath.Join("modules", "documents", "services", "object-storage", resources.ServiceConfigurationName),
		"kind: service\nname: object-storage\nversion: 0.0.0\n"+
			"agent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n")

	before := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	_, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, true)
	if err == nil {
		t.Fatal("expected a refusal rather than an acquisition")
	}
	if !strings.Contains(err.Error(), "acquires nothing") && !strings.Contains(err.Error(), "not materialized") {
		t.Errorf("refusal does not explain the unmaterialized reference: %v", err)
	}
	if readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName)) != before {
		t.Error("the workspace was modified by a refused import")
	}
}

// Provenance records the action as metadata, and does NOT put the one-shot
// directive into the re-run hint, which would fail if anyone re-ran it.
func TestRetireProvenanceKeepsTheRerunHintReusable(t *testing.T) {
	dir := retireWorkspace(t, bareShared)
	if _, err := retireImport(t, dir, []string{"service-config:object-storage=documents/object-storage"}, false); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	content := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	var hint, record string
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.Contains(line, "re-run:"):
			hint = line
		case strings.Contains(line, "operator retired"):
			record = line
		}
	}
	if record == "" {
		t.Error("the retirement was not recorded in provenance")
	}
	if hint == "" {
		t.Fatal("the re-run hint is missing")
	}
	if strings.Contains(hint, "retire-service-key") {
		t.Errorf("the one-shot directive was baked into the re-run hint: %s", hint)
	}
}

var _ = io.Discard
