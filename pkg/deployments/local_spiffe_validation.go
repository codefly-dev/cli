package deployments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const certManagerController = "controller"

type issuerContainer struct {
	Name    string   `json:"name" yaml:"name"`
	Image   string   `json:"image" yaml:"image"`
	Command []string `json:"command" yaml:"command"`
	Args    []string `json:"args" yaml:"args"`
}

type issuerWorkload struct {
	Kind     string `json:"kind" yaml:"kind"`
	Metadata struct {
		Name string `json:"name" yaml:"name"`
	} `json:"metadata" yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers     []issuerContainer `json:"containers" yaml:"containers"`
				InitContainers []issuerContainer `json:"initContainers" yaml:"initContainers"`
			} `json:"spec" yaml:"spec"`
		} `json:"template" yaml:"template"`
	} `json:"spec" yaml:"spec"`
}

func validateExistingCertManager(ctx context.Context, kubectl Kubectl, pins map[string]string) error {
	for _, component := range []string{certManagerController, "webhook", "cainjector"} {
		name := "cert-manager-" + component
		workload := name
		if component == certManagerController {
			workload = "cert-manager"
		}
		// The installation's pin table is the sole source of reviewed images.
		var image string
		for tagged, digest := range pins {
			if strings.HasPrefix(tagged, "quay.io/jetstack/"+name+":") {
				image = tagged + "@" + digest
			}
		}
		if image == "" {
			return fmt.Errorf("no reviewed image for %s", name)
		}
		expected := []issuerContainer{{Name: name, Image: image}}
		if component == certManagerController {
			expected[0].Args = []string{disableDefaultApprover}
		}
		if err := validateIssuerWorkload(ctx, kubectl, "deployment", workload, expected); err != nil {
			return err
		}
	}
	return nil
}

// Read the already verified, digest-pinned install manifest, so the installed
// driver, sidecars and approver are judged against the exact same policy.
func validateExistingSPIFFE(ctx context.Context, kubectl Kubectl, pinned []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(pinned))
	count := 0
	for {
		var expected issuerWorkload
		if err := decoder.Decode(&expected); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode reviewed SPIFFE workload: %w", err)
		}
		if expected.Kind != "Deployment" && expected.Kind != "DaemonSet" {
			continue
		}
		count++
		if err := validateIssuerWorkload(ctx, kubectl, strings.ToLower(expected.Kind), expected.Metadata.Name, expected.Spec.Template.Spec.Containers); err != nil {
			return err
		}
	}
	if count != 2 {
		return fmt.Errorf("reviewed SPIFFE manifest must contain the driver and approver workloads")
	}
	return nil
}

func validateIssuerWorkload(ctx context.Context, kubectl Kubectl, kind, name string, expected []issuerContainer) error {
	raw, err := kubectl(ctx, "", "get", kind, name, "-n", certManagerNamespace, "-o", "json")
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", kind, name, err)
	}
	var live issuerWorkload
	if err := json.Unmarshal([]byte(raw), &live); err != nil {
		return fmt.Errorf("read %s %s: %w", kind, name, err)
	}
	if live.Metadata.Name != name || !strings.EqualFold(live.Kind, kind) {
		return fmt.Errorf("inspect %s %s returned a different resource", kind, name)
	}
	wanted := make(map[string]issuerContainer, len(expected))
	for _, container := range expected {
		wanted[container.Name] = container
	}
	spec := live.Spec.Template.Spec
	for _, container := range spec.InitContainers {
		return fmt.Errorf("%s %s init container %s image %q is not reviewed", kind, name, container.Name, container.Image)
	}
	for _, container := range spec.Containers {
		want, known := wanted[container.Name]
		if !known || container.Image != want.Image {
			return fmt.Errorf("%s %s container %s image %q is not the reviewed image %q", kind, name, container.Name, container.Image, want.Image)
		}
		if len(container.Command) != 0 {
			return fmt.Errorf("%s %s container %s overrides the reviewed command", kind, name, container.Name)
		}
		for _, arg := range want.Args {
			if err := requireIssuerArgument(name, &container, arg); err != nil {
				return err
			}
		}
		delete(wanted, container.Name)
	}
	if len(wanted) > 0 {
		return fmt.Errorf("%s %s is missing reviewed containers", kind, name)
	}
	return nil
}

// Security flags take string values: --flag=value and --flag value are
// equivalent, the last value wins, and -- ends flag parsing. Other flags must
// use = so their arity cannot hide a security flag as an unparsed value. Refuse
// positional/ambiguous arguments rather than guess how an issuer parses them.
func requireIssuerArgument(workload string, container *issuerContainer, expected string) error {
	flag, want, _ := strings.Cut(expected, "=")
	flagName := strings.TrimLeft(flag, "-")
	var effective string
	found := false
	for i := 0; i < len(container.Args); i++ {
		arg := container.Args[i]
		if arg == "--" {
			break
		}
		key, value, inline := strings.Cut(arg, "=")
		if strings.TrimLeft(key, "-") != flagName || !strings.HasPrefix(key, "-") {
			if !strings.HasPrefix(key, "-") || !inline {
				return fmt.Errorf("%s container %s must declare %s; cannot verify ambiguous argument %q (use --name=value for other flags)", workload, container.Name, expected, arg)
			}
			continue
		}
		if !inline {
			i++
			if i == len(container.Args) {
				return fmt.Errorf("%s container %s must declare %s; %s has no value", workload, container.Name, expected, flag)
			}
			value = container.Args[i]
		}
		effective, found = value, true
	}
	if !found || effective != want {
		return fmt.Errorf("%s container %s must declare %s; effective %s value is %q", workload, container.Name, expected, flag, effective)
	}
	return nil
}
