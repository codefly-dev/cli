// Package cellfile is the cell file: what one composition declares about one
// cell, emitted by the render for one environment and read by the cell's
// infrastructure to derive every policy it enforces — namespaces, workload
// identities, the admitted call graph, ingress, egress — instead of
// transcribing them by hand.
//
// The document is the CLI's: it is produced here after the final render and
// validated by its consumer as a published interface from another repository
// (unknown keys refused, every identity re-derived). This package therefore
// carries the shape and a deterministic encoding, and nothing a consumer
// decides. It is a build-time inventory regenerated whole on every render, so
// absence is removal; it carries no generation, no ownership domain and no
// tombstone, which is the opposite of the presence document's rule.
package cellfile

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Schema is the one schema this package writes and reads.
const Schema = "codefly/cell/v1"

// FileName is the file's name wherever it is written.
const FileName = "cell.yaml"

// File is the whole cell: one header naming the host, and one namespace entry
// per module delivered to it, sorted by namespace name.
type File struct {
	Schema      string      `yaml:"schema"`
	Coordinate  string      `yaml:"coordinate"`
	Component   string      `yaml:"component"`
	Environment string      `yaml:"environment"`
	Source      *Source     `yaml:"source,omitempty"`
	Domain      string      `yaml:"domain,omitempty"`
	TrustDomain string      `yaml:"trust_domain"`
	Namespaces  []Namespace `yaml:"namespaces"`
}

// Source is the composition-owned repository, the branch the rendered trees
// are published on, and the revision the render was based on: the commit
// checked out when it ran, which is the parent of the commit that holds the
// rendered trees (equal to it only when the render changed nothing). The
// producer cannot name the commit it will be committed in, so a consumer that
// binds a delivery to an exact commit sets Revision itself when it pins the
// file, and a consumer tracking Branch reads its head.
type Source struct {
	Repo     string `yaml:"repo"`
	Revision string `yaml:"revision"`
	Branch   string `yaml:"branch"`
	// Path is the publication root containing modules/. Absent means deployments.
	Path string `yaml:"path,omitempty"`
}

// Namespace is one module's namespace: what runs in it, the external reach it
// is granted, and the presence carrier rendered for it.
type Namespace struct {
	Name      string     `yaml:"name"`
	Module    string     `yaml:"module"`
	Workloads []Workload `yaml:"workloads,omitempty"`
	Egress    []Egress   `yaml:"egress,omitempty"`
	Delivery  *Delivery  `yaml:"delivery,omitempty"`
}

// Workload is one pod-producing object as rendered: its exact pod selector,
// the account and identity it runs as, the container that authenticates, every
// image by digest, and — for a workload that serves traffic — the endpoints
// it serves and who the composition says calls them.
type Workload struct {
	Name           string            `yaml:"name"`
	Kind           string            `yaml:"kind"`
	Selector       map[string]string `yaml:"selector"`
	Service        string            `yaml:"service"`
	ServiceAccount string            `yaml:"service_account"`
	SPIFFEID       string            `yaml:"spiffe_id"`
	Authenticating string            `yaml:"authenticating"`
	Containers     []Container       `yaml:"containers"`
	InitContainers []Container       `yaml:"init_containers,omitempty"`
	Release        *Release          `yaml:"release,omitempty"`
	Endpoints      []Endpoint        `yaml:"endpoints,omitempty"`
	Ingress        []Ingress         `yaml:"ingress,omitempty"`
	Verifier       bool              `yaml:"verifier,omitempty"`
	Bindings       []string          `yaml:"bindings,omitempty"`
	CloudIdentity  bool              `yaml:"cloud_identity,omitempty"`
}

// Container is one container of a pod template and the image it runs, which
// is always a repository plus an image-manifest digest: the one digest a
// policy compares with a running pod's.
type Container struct {
	Name  string `yaml:"name"`
	Image Image  `yaml:"image"`
}

// Image is a digest-pinned image reference, split so neither half is parsed
// back out of a string by a reader.
type Image struct {
	Repository string `yaml:"repository"`
	Digest     string `yaml:"digest"`
}

// Release is the module release the workload was rendered from, descriptive
// only; the image digest is the identity.
type Release struct {
	Publisher string `yaml:"publisher"`
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
}

// Endpoint is one endpoint a serving workload's service declares. Port is the
// CONTAINER port — where a connection lands after Service resolution, which is
// what the mesh authorizes; ServicePort is the Service's port, used by an edge
// route. Consumers are the services the composition declares as callers, each
// `<module>/<service>`: the edge list a policy is derived from.
type Endpoint struct {
	Name        string   `yaml:"name"`
	API         string   `yaml:"api,omitempty"`
	Port        int      `yaml:"port,omitempty"`
	ServicePort int      `yaml:"service_port,omitempty"`
	Consumers   []string `yaml:"consumers,omitempty"`
	// Presenter marks an endpoint whose listener accepts only a protected
	// waypoint's assertion of the caller: the cell fronts it with a waypoint,
	// admits the waypoint alone on this port, and hands the service the
	// waypoint's identity. Declared by the composition's environment.
	Presenter bool `yaml:"presenter,omitempty"`
}

// Ingress is one edge route onto an endpoint: every host the environment
// routes to it, each once.
type Ingress struct {
	Endpoint string   `yaml:"endpoint"`
	Hosts    []string `yaml:"hosts"`
}

// Egress is the external reach one service is granted: named hosts with their
// ports, and CIDRs a managed service lives in.
type Egress struct {
	Service string       `yaml:"service"`
	Hosts   []EgressHost `yaml:"hosts,omitempty"`
	CIDRs   []string     `yaml:"cidrs,omitempty"`
}

// EgressHost is one external host and port.
type EgressHost struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
}

// Delivery is the presence carrier rendered for a namespace: the Job that
// delivers the module's manifest to the host, declared because a closed
// admission set must know it and the verifier must admit it.
type Delivery struct {
	Kind           string            `yaml:"kind"`
	Selector       map[string]string `yaml:"selector"`
	ServiceAccount string            `yaml:"service_account"`
	SPIFFEID       string            `yaml:"spiffe_id,omitempty"`
	Container      string            `yaml:"container"`
	Image          Image             `yaml:"image"`
}

// Encode writes the file deterministically and reads the bytes back through
// Parse, so a document this writer produced that its own reader would refuse,
// or that would read back as another document, is caught where it is written.
func (file *File) Encode() ([]byte, error) {
	if file == nil {
		return nil, errors.New("no cell file to encode")
	}
	if file.Schema != Schema {
		return nil, fmt.Errorf("cell file schema %q is not %q", file.Schema, Schema)
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(file); err != nil {
		return nil, fmt.Errorf("encode the cell file: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode the cell file: %w", err)
	}
	parsed, err := Parse(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the encoded cell file does not read back: %w", err)
	}
	var again bytes.Buffer
	check := yaml.NewEncoder(&again)
	check.SetIndent(2)
	if err := check.Encode(parsed); err != nil {
		return nil, fmt.Errorf("re-encode the cell file: %w", err)
	}
	if err := check.Close(); err != nil {
		return nil, fmt.Errorf("re-encode the cell file: %w", err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		return nil, errors.New("the cell file does not round-trip through its own reader")
	}
	return out.Bytes(), nil
}

// Parse reads one cell file. An unknown key is refused: the shape is a
// published interface, and a key neither side agreed on is a disagreement,
// not a field to skip.
func Parse(data []byte) (*File, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode the cell file: %w", err)
	}
	if file.Schema != Schema {
		return nil, fmt.Errorf("unknown cell file schema %q; this reader accepts %q only", file.Schema, Schema)
	}
	if len(file.Namespaces) == 0 {
		return nil, errors.New("the cell file names no namespace")
	}
	return &file, nil
}
