package deploysecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
)

// Store is the environment's remote secret store as this package needs it: one
// JSON document of string properties per remote key, the shape an ExternalSecret
// reading `property` from `remoteRef.key` resolves against.
//
// No method returns a value to a caller that prints: Read hands the document to
// the planner, which reports property names only.
type Store interface {
	// Describe reports whether the remote key exists and holds a readable
	// version, without reading it.
	Describe(ctx context.Context, key string) (Description, error)
	// Read returns the remote key's current document.
	Read(ctx context.Context, key string) (map[string]string, error)
	// Write stores document as the remote key's new current version, creating
	// the key when Describe reported it absent.
	Write(ctx context.Context, key string, document map[string]string, create bool) error
	// Name identifies the store in output: its backend and location, never a
	// credential.
	Name() string
}

// Description is what can be known about a remote key without reading it.
type Description struct {
	Exists bool
	// HasVersion is false for a key created with no enabled version: reading it
	// fails, so it is planned as an empty document.
	HasVersion bool
}

// Runner executes a command with stdin and returns its standard output. The
// error it returns must never carry the output: for a read, the output is the
// secret.
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// ExecRunner runs the command on the host. A failure carries the command's
// standard error — which the backends' CLIs never fill with a secret value —
// and never its standard output.
func ExecRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, &CommandError{Command: name + " " + firstArguments(args), Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.Bytes(), nil
}

// CommandError is a failed backend command. It names the command by its verb
// only: arguments may name remote keys, never values, but the verb is enough to
// act on.
type CommandError struct {
	Command string
	Stderr  string
	Err     error
}

func (e *CommandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Command, e.Err, e.Stderr)
}

func (e *CommandError) Unwrap() error { return e.Err }

func firstArguments(args []string) string {
	var verb []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || len(verb) == 3 {
			break
		}
		verb = append(verb, arg)
	}
	return strings.Join(verb, " ")
}

// ClusterTarget is the cluster the environment's ExternalSecrets run in, where
// the store they name is declared.
type ClusterTarget struct {
	Kubeconfig string
	Context    string
}

// ResolveStore resolves the backend behind the environment's secret store
// reference by reading the SecretStore or ClusterSecretStore the render names
// from the cluster: that object is what the External Secrets Operator resolves
// the same reference through, so it is the one place the backend is declared.
// Nothing about the backend is configured here.
func ResolveStore(ctx context.Context, run Runner, target ClusterTarget, ref environments.EnvironmentSecretStoreReference, namespace string) (Store, error) {
	args := make([]string, 0, 12)
	switch ref.Kind {
	case "ClusterSecretStore":
		args = append(args, "get", "clustersecretstore", ref.Name)
	case "SecretStore":
		args = append(args, "get", "secretstore", ref.Name, "--namespace", namespace)
	default:
		return nil, fmt.Errorf("secret store kind %q must be SecretStore or ClusterSecretStore", ref.Kind)
	}
	args = append(args, "--kubeconfig", target.Kubeconfig, "--context", target.Context, "-o", "json")
	output, err := run(ctx, nil, "kubectl", args...)
	if err != nil {
		return nil, fmt.Errorf("read %s %s from context %s: %w", ref.Kind, ref.Name, target.Context, err)
	}
	var store struct {
		Spec struct {
			Provider map[string]json.RawMessage `json:"provider"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(output, &store); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", ref.Kind, ref.Name, err)
	}
	if raw, ok := store.Spec.Provider["gcpsm"]; ok {
		var gcp struct {
			ProjectID string `json:"projectID"`
		}
		if err := json.Unmarshal(raw, &gcp); err != nil {
			return nil, fmt.Errorf("decode %s %s gcpsm provider: %w", ref.Kind, ref.Name, err)
		}
		if gcp.ProjectID == "" {
			return nil, fmt.Errorf("%s %s declares no gcpsm projectID", ref.Kind, ref.Name)
		}
		return &GoogleSecretManager{Project: gcp.ProjectID, Run: run}, nil
	}
	providers := make([]string, 0, len(store.Spec.Provider))
	for provider := range store.Spec.Provider {
		providers = append(providers, provider)
	}
	return nil, fmt.Errorf("%s %s resolves through provider %v, which `codefly deploy secrets` cannot write yet (supported: gcpsm)", ref.Kind, ref.Name, providers)
}

// GoogleSecretManager is a gcpsm-backed store, driven through the operator's own
// gcloud login: the verb acts with exactly the authority the operator holds.
type GoogleSecretManager struct {
	Project string
	Run     Runner
}

func (store *GoogleSecretManager) Name() string {
	return "gcpsm project " + store.Project
}

func (store *GoogleSecretManager) Describe(ctx context.Context, key string) (Description, error) {
	if _, err := store.Run(ctx, nil, "gcloud", "secrets", "describe", key, "--project", store.Project, "--format=value(name)"); err != nil {
		if notFound(err) {
			return Description{}, nil
		}
		return Description{}, err
	}
	versions, err := store.Run(ctx, nil, "gcloud", "secrets", "versions", "list", key, "--project", store.Project,
		"--filter=state:ENABLED", "--limit=1", "--format=value(name)")
	if err != nil {
		return Description{}, err
	}
	return Description{Exists: true, HasVersion: strings.TrimSpace(string(versions)) != ""}, nil
}

func (store *GoogleSecretManager) Read(ctx context.Context, key string) (map[string]string, error) {
	payload, err := store.Run(ctx, nil, "gcloud", "secrets", "versions", "access", "latest", "--secret", key, "--project", store.Project)
	if err != nil {
		return nil, err
	}
	return decodeDocument(key, payload)
}

func (store *GoogleSecretManager) Write(ctx context.Context, key string, document map[string]string, create bool) error {
	payload, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if create {
		// The replication policy is Secret Manager's default; the store's other
		// secrets use it too, and nothing in the environment declares otherwise.
		if _, err = store.Run(ctx, nil, "gcloud", "secrets", "create", key, "--project", store.Project, "--replication-policy=automatic"); err != nil {
			return err
		}
	}
	// The value travels on stdin, never on the command line where the process
	// table would show it.
	_, err = store.Run(ctx, payload, "gcloud", "secrets", "versions", "add", key, "--project", store.Project, "--data-file=-")
	return err
}

func notFound(err error) bool {
	var command *CommandError
	return errors.As(err, &command) && (strings.Contains(command.Stderr, "NOT_FOUND") || strings.Contains(command.Stderr, "not found"))
}

// decodeDocument parses a remote value as the JSON object of string properties
// every rendered ExternalSecret reads by property. The error never quotes the
// payload.
func decodeDocument(key string, payload []byte) (map[string]string, error) {
	document := map[string]string{}
	if len(bytes.TrimSpace(payload)) == 0 {
		return document, nil
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("remote key %s does not hold a JSON object of string properties", key)
	}
	return document, nil
}
