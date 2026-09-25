package deploysecrets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
)

type call struct {
	stdin string
	name  string
	args  []string
}

// scriptedRunner answers each command by its first arguments and records every
// call.
type scriptedRunner struct {
	calls   []call
	answers map[string]func() ([]byte, error)
}

func (runner *scriptedRunner) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, call{stdin: string(stdin), name: name, args: args})
	answer, ok := runner.answers[name+" "+firstArguments(args)]
	if !ok {
		return nil, errors.New("unexpected command " + name + " " + strings.Join(args, " "))
	}
	return answer()
}

func TestResolveStoreReadsTheBackendFromTheClusterSecretStore(t *testing.T) {
	runner := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"kubectl get clustersecretstore cell-secrets": func() ([]byte, error) {
			return []byte(`{"spec":{"provider":{"gcpsm":{"projectID":"example-project","auth":{}}}}}`), nil
		},
	}}
	store, err := ResolveStore(context.Background(), runner.run, ClusterTarget{Kubeconfig: "/kube", Context: "cell"},
		environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}, "ns")
	if err != nil {
		t.Fatal(err)
	}
	gcp, ok := store.(*GoogleSecretManager)
	if !ok || gcp.Project != "example-project" {
		t.Fatalf("store = %#v, want gcpsm example-project", store)
	}
	args := runner.calls[0].args
	for _, want := range []string{"--context", "cell", "clustersecretstore", "cell-secrets"} {
		if !slices.Contains(args, want) {
			t.Errorf("kubectl args %v lack %s", args, want)
		}
	}
	if slices.Contains(args, "--namespace") {
		t.Error("a cluster-scoped store was read from a namespace")
	}
}

func TestResolveStoreRefusesAProviderItCannotWrite(t *testing.T) {
	runner := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"kubectl get secretstore s": func() ([]byte, error) { return []byte(`{"spec":{"provider":{"vault":{}}}}`), nil },
	}}
	_, err := ResolveStore(context.Background(), runner.run, ClusterTarget{Kubeconfig: "/kube", Context: "cell"},
		environments.EnvironmentSecretStoreReference{Name: "s", Kind: "SecretStore"}, "ns")
	if err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("ResolveStore = %v, want a refusal naming the provider", err)
	}
	if !slices.Contains(runner.calls[0].args, "--namespace") {
		t.Error("a namespaced store was not read from its namespace")
	}
}

func TestGoogleSecretManagerDescribesAMissingKey(t *testing.T) {
	runner := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"gcloud secrets describe key": func() ([]byte, error) {
			return nil, &CommandError{Command: "gcloud secrets describe", Stderr: "ERROR: NOT_FOUND: Secret [key] not found", Err: errors.New("exit status 1")}
		},
	}}
	store := &GoogleSecretManager{Project: "p", Run: runner.run}
	description, err := store.Describe(context.Background(), "key")
	if err != nil || description.Exists {
		t.Fatalf("Describe = %+v, %v, want absent", description, err)
	}
}

func TestGoogleSecretManagerDescribesAKeyWithoutReadingIt(t *testing.T) {
	runner := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"gcloud secrets describe key":  func() ([]byte, error) { return []byte("projects/p/secrets/key\n"), nil },
		"gcloud secrets versions list": func() ([]byte, error) { return []byte(""), nil },
	}}
	store := &GoogleSecretManager{Project: "p", Run: runner.run}
	description, err := store.Describe(context.Background(), "key")
	if err != nil || !description.Exists || description.HasVersion {
		t.Fatalf("Describe = %+v, %v, want exists with no enabled version", description, err)
	}
	for _, recorded := range runner.calls {
		if slices.Contains(recorded.args, "access") {
			t.Fatal("Describe accessed a secret version")
		}
	}
}

// A write creates a missing key, then adds the document as a version through
// stdin: the value never appears on a command line.
func TestGoogleSecretManagerWritesThroughStdin(t *testing.T) {
	runner := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"gcloud secrets create key":   func() ([]byte, error) { return nil, nil },
		"gcloud secrets versions add": func() ([]byte, error) { return nil, nil },
	}}
	store := &GoogleSecretManager{Project: "p", Run: runner.run}
	if err := store.Write(context.Background(), "key", map[string]string{"A": "value-a"}, true); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0].args[1] != "create" {
		t.Fatalf("calls = %+v, want create then versions add", runner.calls)
	}
	add := runner.calls[1]
	if add.stdin != `{"A":"value-a"}` || !slices.Contains(add.args, "--data-file=-") {
		t.Errorf("versions add = %+v, want the JSON document on stdin", add)
	}
	for _, recorded := range runner.calls {
		if strings.Contains(strings.Join(recorded.args, " "), "value-a") {
			t.Error("a value reached the command line")
		}
	}
}

func TestDecodeDocumentNeverQuotesThePayload(t *testing.T) {
	_, err := decodeDocument("key", []byte("not-json-secret-material"))
	if err == nil || strings.Contains(err.Error(), "secret-material") {
		t.Fatalf("decodeDocument = %v, want an error that does not quote the payload", err)
	}
	document, err := decodeDocument("key", []byte(`{"A":"b"}`))
	if err != nil || document["A"] != "b" {
		t.Fatalf("decodeDocument = %v, %v", document, err)
	}
}
