package deployment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,252}$`)
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var imagePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]*[a-z0-9])?)(:[1-9][0-9]{0,4})?/[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var labelPart = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)

// Validated retains private canonical bytes. Accessors return fresh copies, so
// mutation of caller buffers or decoded objects cannot change a compiled result.
// Checked is an inventory whose INTRINSIC form has been accepted: its schema
// version, its required vocabulary and its structural rules. It carries the
// canonical bytes, their digest, and the seven-key projection.
//
// It exists as its own type so that the projection is reachable WITHOUT a
// validation context. The projection is a pure function of a schema-valid
// inventory — namespace, selector, service account, authenticating container,
// the container-name-to-image map and the application/init membership all come
// from Workloads alone. A consumer holding an inventory that was already
// approved, but no longer holding the retained context that approval was
// checked against, must still be able to ask the contract for its rows; if it
// cannot, it writes its own traversal of Workloads[].Template.Spec, which is a
// second implementation of the projection and the exact duplication this
// module exists to prevent.
//
// The type is the gate: Rows is a method, so there is no way to obtain a
// projection without having passed the schema check that produces the value.
type Checked struct {
	canonical   []byte
	rows        []byte
	projections []byte
}

func (c *Checked) Canonical() []byte { return bytes.Clone(c.canonical) }
func (c *Checked) Digest() string    { return Digest(c.canonical) }
func (c *Checked) Inventory() Inventory {
	var i Inventory
	_ = json.Unmarshal(c.canonical, &i)
	return i
}

// Rows is the seven-key projection the cluster's admission policy compares:
// one row per workload with ns, labels, sa, container, images, app and init.
func (c *Checked) Rows() []Row { var r []Row; _ = json.Unmarshal(c.rows, &r); return r }

// Projections is Rows with each row's workload id and delivery member, so a
// consumer selects by name instead of by position.
func (c *Checked) Projections() []Projection {
	var p []Projection
	_ = json.Unmarshal(c.projections, &p)
	return p
}

// Validated is a Checked inventory whose EXTERNAL references and image
// evidence have also been accepted against an independently authenticated
// context. Validation is an offline consistency check and is NEVER an
// authorization decision.
type Validated struct {
	*Checked
}

// Check accepts an inventory's intrinsic form and returns its canonical bytes,
// digest and projection. It consults no context, fetches nothing, and
// authorizes nothing.
func Check(input []byte) (*Checked, error) {
	inv, err := decode[Inventory](input)
	if err != nil {
		return nil, err
	}
	if err = intrinsic(inv); err != nil {
		return nil, err
	}
	return checked(inv, input)
}

// checked builds the value once, so Check and Validate cannot disagree about
// canonical bytes or the projection.
func checked(inv Inventory, input []byte) (*Checked, error) {
	canonical, err := CanonicalJSON(input)
	if err != nil {
		return nil, err
	}
	projections := make([]Projection, 0, len(inv.Workloads))
	rows := make([]Row, 0, len(inv.Workloads))
	for _, w := range inv.Workloads {
		r := Row{w.Controller.Namespace, w.Selector, w.Template.Spec.ServiceAccountName, w.AuthenticatingContainer, map[string]string{}, nil, nil}
		for _, c := range w.Template.Spec.Containers {
			r.App = append(r.App, c.Name)
			r.Images[c.Name] = c.Image
		}
		for _, c := range w.Template.Spec.InitContainers {
			r.Init = append(r.Init, c.Name)
			r.Images[c.Name] = c.Image
		}
		rows = append(rows, r)
		projections = append(projections, Projection{w.ID, w.MemberBinding, r})
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	identified, err := json.Marshal(projections)
	if err != nil {
		return nil, err
	}
	return &Checked{canonical, encoded, identified}, nil
}

// Validate is an offline consistency check, not an authorization decision. The
// caller must independently authenticate contextBytes and fetch retained blobs.
func Validate(inventoryBytes, contextBytes []byte) (*Validated, error) {
	inv, err := decode[Inventory](inventoryBytes)
	if err != nil {
		return nil, err
	}
	if err = intrinsic(inv); err != nil {
		return nil, err
	}
	ctx, err := decode[Context](contextBytes)
	if err != nil {
		return nil, err
	}
	profiles, err := loadProfiles(inv.PlatformRefs, ctx)
	if err != nil {
		return nil, err
	}
	if err = validateReferences(inv, profiles); err != nil {
		return nil, err
	}
	if err = validateImages(inv, profiles.execution, ctx.Blobs); err != nil {
		return nil, err
	}
	base, err := checked(inv, inventoryBytes)
	if err != nil {
		return nil, err
	}
	return &Validated{base}, nil
}

// CheckSchema evaluates the structural AND required codeflyRules vocabulary.
// External reference/evidence checks require Validate with its separate context.
func CheckSchema(input []byte) error {
	_, err := Check(input)
	return err
}
func intrinsic(inv Inventory) error {
	if inv.Schema != Schema {
		return refuse("SCHEMA_VERSION", "$/schema", "unsupported inventory schema")
	}
	if !inv.Complete {
		return refuse("AGGREGATE_COMPLETE", "$/complete", "partial inventories are refused")
	}
	for _, v := range []string{inv.Target.Coordinate, inv.Target.HostComponent, inv.Target.ClusterInstance, inv.Delivery, inv.OwnershipDomain, inv.EnvelopeRevision} {
		if !idPattern.MatchString(v) {
			return refuse("IDENTIFIER", "$", "empty or invalid scope identifier")
		}
	}
	if inv.Generation < 1 || (inv.Generation == 1) != (inv.Previous == nil) || (inv.Previous != nil && (inv.Previous.Generation != inv.Generation-1 || !digestPattern.MatchString(inv.Previous.Digest))) {
		return refuse("AGGREGATE_PREVIOUS", "$/previous", "generation must advance an exact predecessor; generation 1 has null previous")
	}
	workloadIDs := map[string]bool{}
	objects := map[ObjectIdentity]bool{}
	for i, w := range inv.Workloads {
		p := fmt.Sprintf("$/workloads/%d", i)
		if !idPattern.MatchString(w.ID) || workloadIDs[w.ID] {
			return refuse("WORKLOAD_ID", p+"/id", "invalid or duplicate workload ID")
		}
		workloadIDs[w.ID] = true
		if objects[w.Controller] {
			return refuse("OBJECT_UNIQUE", p+"/controller", "duplicate controller identity")
		}
		objects[w.Controller] = true
		if err := controller(w.Controller, p); err != nil {
			return err
		}
		if len(w.Selector) == 0 {
			return refuse("SELECTOR_EMPTY", p+"/selector", "equality selector must not be empty")
		}
		for _, key := range sortedKeys(w.Selector) {
			value := w.Selector[key]
			if !labelKey(key) || !labelValue(value) {
				return refuse("LABEL", p+"/selector", "invalid label")
			}
			if v, ok := w.Template.Metadata.Labels[key]; !ok || v != value {
				return refuse("SELECTOR_TEMPLATE", p+"/selector", "selector must be present in template labels")
			}
		}
		for j := 0; j < i; j++ {
			other := inv.Workloads[j]
			if w.Controller.Namespace == other.Controller.Namespace && overlap(w.Selector, other.Selector) {
				return refuse("SELECTOR_OVERLAP", p+"/selector", "selectors overlap in the same namespace, including carriers")
			}
		}
		if (w.Artifact.Artifact == nil) == (w.Artifact.CarrierProfile == nil) {
			return refuse("WORKLOAD_ORIGIN", p+"/artifact", "exactly one artifact or carrier profile is required")
		}
		if !slices.Contains([]string{"none", "application", "delivery", "platform"}, w.CredentialKind) {
			return refuse("CREDENTIAL_KIND", p+"/credential_kind", "unsupported credential kind")
		}
		if w.CredentialKind == "none" {
			if w.AuthenticatingContainer != nil || w.Identity != nil {
				return refuse("AUTHENTICATING_CONTAINER", p, "ineligible execution must have null container and identity")
			}
		} else if w.AuthenticatingContainer == nil || w.Identity == nil {
			return refuse("AUTHENTICATING_CONTAINER", p, "credential-bearing execution requires container and identity")
		}
		if err := template(w, p); err != nil {
			return err
		}
	}
	for i, m := range inv.Members {
		if m.Release != nil && (!idPattern.MatchString(m.Release.Publisher) || !idPattern.MatchString(m.Release.Name) || !versionPattern.MatchString(m.Release.Version)) {
			return refuse("RELEASE_METADATA", fmt.Sprintf("$/members/%d/release", i), "present release coordinates must be well formed")
		}
	}
	return nil
}
func overlap(a, b map[string]string) bool {
	for k, v := range a {
		if other, ok := b[k]; ok && v != other {
			return false
		}
	}
	return true
}
func controller(o ObjectIdentity, p string) error {
	version := "apps/v1"
	if o.Kind == "Job" || o.Kind == "CronJob" {
		version = "batch/v1"
	}
	if !slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}, o.Kind) || o.APIVersion != version {
		return refuse("CONTROLLER_KIND", p+"/controller", "unsupported Kubernetes 1.34 controller")
	}
	if !dnsName(o.Namespace) || !dnsSubdomain(o.Name) {
		return refuse("OBJECT_IDENTITY", p+"/controller", "invalid namespace or object name")
	}
	return nil
}
func dnsName(s string) bool { return len(s) > 0 && len(s) <= 63 && namePattern.MatchString(s) }
func dnsSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !dnsName(part) {
			return false
		}
	}
	return true
}
func labelKey(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return false
	}
	if len(parts) == 2 && !dnsSubdomain(parts[0]) {
		return false
	}
	last := parts[len(parts)-1]
	return len(last) <= 63 && labelPart.MatchString(last)
}
func labelValue(s string) bool { return len(s) <= 63 && (s == "" || labelPart.MatchString(s)) }
func cleanRelative(s string) bool {
	return s != "" && s != "." && path.Clean(s) == s && !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "../") && !strings.ContainsAny(s, "\\%:\x00")
}
func template(w Workload, p string) error {
	spec := w.Template.Spec
	for _, key := range sortedKeys(w.Template.Metadata.Labels) {
		if !labelKey(key) || !labelValue(w.Template.Metadata.Labels[key]) {
			return refuse("LABEL", p+"/template", "invalid template label")
		}
	}
	for _, key := range sortedKeys(w.Template.Metadata.Annotations) {
		if !labelKey(key) {
			return refuse("ANNOTATION", p+"/template", "invalid annotation key")
		}
	}
	if !dnsSubdomain(spec.ServiceAccountName) || spec.AutomountServiceAccountToken {
		return refuse("SERVICE_ACCOUNT", p+"/template/spec", "explicit service account and disabled automatic token mounting required")
	}
	if !slices.Contains([]string{"Always", "Never", "OnFailure"}, spec.RestartPolicy) || ((w.Controller.Kind != "Job" && w.Controller.Kind != "CronJob") && spec.RestartPolicy != "Always") || ((w.Controller.Kind == "Job" || w.Controller.Kind == "CronJob") && spec.RestartPolicy == "Always") {
		return refuse("RESTART_POLICY", p+"/template/spec/restartPolicy", "restart policy does not match controller")
	}
	if spec.DNSPolicy != "ClusterFirst" || spec.SchedulerName != "default-scheduler" || spec.EnableServiceLinks || spec.TerminationGracePeriodSeconds < 0 || spec.TerminationGracePeriodSeconds > 30 {
		return refuse("POD_UNSUPPORTED", p+"/template/spec", "unsupported DNS, scheduler, service links or grace period")
	}
	if spec.OS.Name != "linux" || len(spec.NodeSelector) != 2 || spec.NodeSelector["kubernetes.io/os"] != spec.OS.Name || !slices.Contains([]string{"amd64", "arm64"}, spec.NodeSelector["kubernetes.io/arch"]) {
		return refuse("POD_PLATFORM", p+"/template/spec", "explicit linux/amd64 or linux/arm64 node selection is required")
	}
	if len(spec.Containers) == 0 {
		return refuse("CONTAINER_REQUIRED", p+"/template/spec/containers", "application containers must not be empty")
	}
	names := map[string]bool{}
	ports := map[string]bool{}
	authFound := false
	for role, containers := range [][]Container{spec.Containers, spec.InitContainers} {
		for _, c := range containers {
			if !dnsName(c.Name) || names[c.Name] {
				return refuse("CONTAINER_UNIQUE", p+"/template/spec", "container names must be unique across application and init positions")
			}
			names[c.Name] = true
			if w.AuthenticatingContainer != nil && c.Name == *w.AuthenticatingContainer {
				if role != 0 {
					return refuse("AUTHENTICATING_CONTAINER", p, "authenticating container must be in application position")
				}
				authFound = true
			}
			if !completeImage(c.Image) {
				return refuse("IMAGE_REFERENCE", p+"/template/spec", "complete registry/repository@sha256 manifest reference required; tags refused")
			}
			if c.ImagePullPolicy != "Always" && c.ImagePullPolicy != "IfNotPresent" {
				return refuse("CONTAINER_UNSUPPORTED", p, "unsupported image pull policy")
			}
			if c.RestartPolicy != nil && (role != 1 || *c.RestartPolicy != "Always") {
				return refuse("RESTART_POLICY", p, "only init containers may specify restartPolicy Always")
			}
			if c.TerminationMessagePath != "/dev/termination-log" || c.TerminationMessagePolicy != "File" || (c.WorkingDir != "" && (!strings.HasPrefix(c.WorkingDir, "/") || path.Clean(c.WorkingDir) != c.WorkingDir)) {
				return refuse("CONTAINER_UNSUPPORTED", p, "unsupported termination settings or working directory")
			}
			sc := c.SecurityContext
			if !sc.RunAsNonRoot || sc.RunAsUser < 1 || sc.RunAsGroup < 1 || !sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation || sc.Privileged || !reflect.DeepEqual(sc.Capabilities.Drop, []string{"ALL"}) || len(sc.Capabilities.Add) != 0 || sc.SeccompProfile.Type != "RuntimeDefault" {
				return refuse("CONTAINER_SECURITY", p, "unsupported container security context")
			}
			envNames := map[string]bool{}
			for _, env := range c.Env {
				if !envPattern.MatchString(env.Name) || envNames[env.Name] {
					return refuse("CONFIGURATION_USE", p, "invalid or duplicate environment variable")
				}
				envNames[env.Name] = true
				if (env.Value == nil) == (env.ValueFrom == nil) {
					return refuse("CONFIGURATION_USE", p, "exactly one environment value source required")
				}
				if env.ValueFrom != nil {
					src := env.ValueFrom
					if (src.ConfigMapKeyRef == nil) == (src.SecretKeyRef == nil) {
						return refuse("CONFIGURATION_USE", p, "exactly one key reference required")
					}
					key := src.ConfigMapKeyRef
					if key == nil {
						key = src.SecretKeyRef
					}
					if !dnsSubdomain(key.Name) || key.Key == "" || key.Optional {
						return refuse("CONFIGURATION_USE", p, "invalid or optional configuration source")
					}
				}
			}
			for _, port := range c.Ports {
				if !dnsName(port.Name) || len(port.Name) > 15 || ports[port.Name] || !validPort(port.Protocol, port.ContainerPort) {
					return refuse("ENDPOINT", p, "invalid or duplicate named container port")
				}
				ports[port.Name] = true
			}
		}
	}
	if w.AuthenticatingContainer != nil && !authFound {
		return refuse("AUTHENTICATING_CONTAINER", p, "designated application container missing")
	}
	return nil
}
func validPort(protocol string, port int64) bool {
	return (protocol == "TCP" || protocol == "UDP") && port > 0 && port <= 65535
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func completeImage(reference string) bool {
	if !imagePattern.MatchString(reference) {
		return false
	}
	registry := strings.SplitN(reference, "/", 2)[0]
	host := registry
	if index := strings.LastIndex(registry, ":"); index >= 0 {
		host = registry[:index]
		port, err := strconv.Atoi(registry[index+1:])
		if err != nil || port > 65535 {
			return false
		}
	} else if !strings.Contains(host, ".") && host != "localhost" {
		return false
	}
	return dnsSubdomain(host)
}
