package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- Delivery Jobs ---
//
// A delivery document declares; a delivery Job delivers. The documents a render
// writes are desired state Argo keeps true in the cluster, and nothing more:
// the host learns of a generation when its delivery API is POSTed the signed
// carrier, which is what these Jobs do. One presence Job per module tree, run
// in the module's own namespace, POSTs every presence carrier the tree
// delivers; one authority Job, run in the platform's authority namespace —
// which only the delivery pipeline may create Jobs in — POSTs every authority
// carrier.
//
// The Jobs are Argo CD Sync hooks rather than tracked resources, on purpose. A
// tracked Job is applied once and, being Complete, never re-synced: restore the
// host's database to before a generation and the applied state is gone while
// nothing re-POSTs it. A hook runs on every sync of the Application, from state
// that still exists — the ConfigMaps — so a replay after a restore is an
// ordinary `argocd app sync`, and a sync that changes nothing else re-POSTs a
// generation the host answers "current" to. BeforeHookCreation deletes the
// previous run before the next, so completed Jobs do not accumulate.
//
// Reachability is a retry, not a sync wave. A per-module Application cannot
// express "after the host's delivery API is reachable", because the host is
// another module's Application; so the Job retries a transport failure or a
// 5xx with backoff until the API answers. What the wave orders is narrower and
// exact. The Job lives in the bindings (or authority) Application beside the
// ConfigMaps it mounts and the ServiceAccount it runs as, which sit at the
// default wave; it is annotated one wave later so it runs once they exist.
// That Application is itself generated in the module-resources wave, before
// any unit's Application, so delivery is never held behind the readiness of
// workloads that may need the presence it delivers in order to become ready —
// a later placement would be a deadlock the host's "no applied generation yet"
// retry could not break.
//
// A terminal answer — a stale or rewritten generation (409), a document the
// host refuses (422), a carrier it does not accept (401, 403) — fails the Job,
// and with it the sync, so a refused delivery is visible where it happened
// rather than discovered when a solution never appears.

const (
	// deliveryImage is the image the delivery Jobs run: a digest-pinned curl.
	// It is the second third-party image the render itself pins (remoteImage
	// is the first), and it is pinned the same way, by digest, so that what a
	// Job runs is what was reviewed. A codefly-owned delivery image would
	// replace it; until one is published, the pinned digest is the honest
	// record of what delivers.
	deliveryImage = "curlimages/curl:8.18.0@sha256:d94d07ba9e7d6de898b6d96c1a072f6f8266c687af78a74f380087a0addf5d17"

	// deliveryContainerName is the one container of a delivery Job.
	deliveryContainerName = "deliver"

	// deliveryServiceAccount is the ServiceAccount a delivery Job runs as. In a
	// module's namespace the render creates it; in the authority namespace the
	// platform does, because that namespace is not the module's to populate.
	deliveryServiceAccount = "delivery"

	// authorityNamespace is the namespace authority Jobs run in. The platform
	// provisions it so that only the delivery pipeline may create Jobs there
	// (the writer-trust decision: signed AND isolated).
	authorityNamespace = "platform-authority"

	// presenceDeliveryPath and authorityDeliveryPath are the host's delivery API
	// routes, relative to the delivery endpoint the environment names.
	presenceDeliveryPath  = "/platform/_delivery/presence"
	authorityDeliveryPath = "/platform/_delivery/authority"

	// presenceCarrierKey and authorityCarrierKey are the ConfigMap data keys
	// holding the signed carrier of each document type: the canonical bytes that
	// were signed, verbatim, beside the Sigstore bundle over them. The carrier is
	// JSON under a YAML extension because a JSON object is valid YAML and a YAML
	// emitter would fold the long scalar the signature covers.
	presenceCarrierKey  = "solution-host-binding.signed.codefly.yaml"
	authorityCarrierKey = "solution-authority.signed.codefly.yaml"

	// deliveryLabel marks the objects that deliver documents, by document type.
	deliveryLabel     = "codefly.dev/delivery"
	deliveryPresence  = "presence"
	deliveryAuthority = "authority"

	deliveryDocumentsMount = "/delivery/documents"
	deliveryTokenMount     = "/var/run/secrets/codefly/delivery" //nolint:gosec // a mount path, not a credential
	deliveryTokenFile      = "token"
	// deliveryTokenSeconds is the projected token's lifetime. The kubelet
	// rotates it at 80%, and the Job re-reads the file on every request, so a
	// long retry never presents an expired token.
	deliveryTokenSeconds = 600
	// deliveryDeadlineSeconds bounds one Job run: long enough for the host to
	// come up behind it, short enough that a sync does not hang for ever.
	deliveryDeadlineSeconds = 1800
	deliveryBackoffLimit    = 6

	argoHookAnnotation         = "argocd.argoproj.io/hook"
	argoHookDeletePolicy       = "argocd.argoproj.io/hook-delete-policy"
	argoSyncWaveAnnotation     = "argocd.argoproj.io/sync-wave"
	argoHookSync               = "Sync"
	argoHookBeforeHookCreation = "BeforeHookCreation"
	kindServiceAccount         = "ServiceAccount"
)

// DeliveryTarget is the host's delivery API as this render resolved it: the
// in-cluster base address of the endpoint the environment's host block names.
type DeliveryTarget struct {
	// URL is "<scheme>://<service>.<namespace>.svc.cluster.local:<port>".
	URL string
	// Audience is the audience the host verifies a carrier token against; the
	// Job's projected ServiceAccount token is minted for it.
	Audience string
}

// deliveryDocument is one carrier a Job delivers: the ConfigMap holding it and
// the key the carrier is under.
type deliveryDocument struct {
	ConfigMap string
	Key       string
	// Name is the document's file name inside the Job's documents mount.
	Name string
}

// deliveryJobManifest is the Job a render writes, as a struct so the delivered
// YAML has a stable field order.
type deliveryJobManifest struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   deliveryJobMetadata `yaml:"metadata"`
	Spec       deliveryJobSpec     `yaml:"spec"`
}

type deliveryJobMetadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type deliveryJobSpec struct {
	BackoffLimit          int                 `yaml:"backoffLimit"`
	ActiveDeadlineSeconds int                 `yaml:"activeDeadlineSeconds"`
	Template              deliveryPodTemplate `yaml:"template"`
}

type deliveryPodTemplate struct {
	Metadata struct {
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec deliveryPodSpec `yaml:"spec"`
}

type deliveryPodSpec struct {
	ServiceAccountName           string              `yaml:"serviceAccountName"`
	AutomountServiceAccountToken bool                `yaml:"automountServiceAccountToken"`
	RestartPolicy                string              `yaml:"restartPolicy"`
	SecurityContext              map[string]any      `yaml:"securityContext"`
	Containers                   []deliveryContainer `yaml:"containers"`
	Volumes                      []deliveryVolume    `yaml:"volumes"`
}

type deliveryVolume struct {
	Name      string            `yaml:"name"`
	Projected deliveryProjected `yaml:"projected"`
}

type deliveryProjected struct {
	Sources []deliveryProjectedSource `yaml:"sources"`
}

type deliveryTokenSource struct {
	Audience          string `yaml:"audience"`
	ExpirationSeconds int    `yaml:"expirationSeconds"`
	Path              string `yaml:"path"`
}

type deliveryContainer struct {
	Name            string                `yaml:"name"`
	Image           string                `yaml:"image"`
	Command         []string              `yaml:"command"`
	Env             []deliveryEnv         `yaml:"env"`
	VolumeMounts    []deliveryVolumeMount `yaml:"volumeMounts"`
	Resources       deliveryResources     `yaml:"resources"`
	SecurityContext map[string]any        `yaml:"securityContext"`
}

type deliveryEnv struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type deliveryVolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly"`
}

type deliveryResources struct {
	Requests deliveryAmounts `yaml:"requests"`
	Limits   deliveryAmounts `yaml:"limits"`
}

type deliveryAmounts struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// deliveryProjectedSource is one source of a projected volume: a ConfigMap key
// projected under the document's file name, or the token minted for the
// host's audience.
type deliveryProjectedSource struct {
	ConfigMap           *deliveryConfigMapSource `yaml:"configMap,omitempty"`
	ServiceAccountToken *deliveryTokenSource     `yaml:"serviceAccountToken,omitempty"`
}

type deliveryConfigMapSource struct {
	Name  string              `yaml:"name"`
	Items []deliveryKeyToPath `yaml:"items"`
}

type deliveryKeyToPath struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
}

// deliveryServiceAccountManifest is the account a delivery Job runs as.
type deliveryServiceAccountManifest struct {
	APIVersion                   string                  `yaml:"apiVersion"`
	Kind                         string                  `yaml:"kind"`
	Metadata                     deliveryAccountMetadata `yaml:"metadata"`
	AutomountServiceAccountToken bool                    `yaml:"automountServiceAccountToken"`
}

type deliveryAccountMetadata struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
}

// deliveryScript is what the Job runs: POST every carrier in the documents
// mount to the delivery route, re-reading the projected token per request,
// retrying what may succeed later and failing on what will not — after every
// document has been offered, so one refusal never withholds the rest, a
// tombstone included.
//
// A 2xx is a committed delivery and its body the receipt. 400, 401, 403, 409
// and 422 are the host's verdicts on this document or this carrier, and a
// retry would ask the same question of the same host: the document is
// refused, the next one is posted, and the Job fails at the end. Anything
// else — no connection, a 5xx, a 429, a 404 from a host a version behind —
// is the host not being there yet, which is what the backoff is for.
//
// Shell variables are spelled $NAME and never ${NAME}: the promotable ruleset
// refuses a manifest carrying ${…} as an unresolved placeholder, and this
// script is a manifest. The receipt is read from curl's output rather than a
// file, so the container needs no writable filesystem.
const deliveryScript = `set -eu
url="$DELIVERY_URL$DELIVERY_PATH"
delivered=0
refused=0
for file in "$DELIVERY_DOCUMENTS"/*.json; do
  [ -e "$file" ] || { echo "no document to deliver under $DELIVERY_DOCUMENTS"; exit 1; }
  name=$(basename "$file")
  attempt=0
  while :; do
    answer=$(curl -sS --max-time 30 -w '\n%{http_code}' -X POST \
      -H "Authorization: Bearer $(cat "$DELIVERY_IDENTITY_FILE")" \
      -H 'Content-Type: application/json' \
      --data-binary @"$file" "$url") || answer="
000"
    code=$(printf '%s' "$answer" | tail -n 1)
    receipt=$(printf '%s' "$answer" | sed '$d')
    case "$code" in
      2??) echo "delivered $name: $receipt"; delivered=$((delivered + 1)); break ;;
      400|401|403|409|422) echo "refused $name with $code: $receipt"; refused=$((refused + 1)); break ;;
      *) attempt=$((attempt + 1))
         if [ "$attempt" -ge 30 ]; then echo "giving up on $name after $attempt attempts (last answer $code)"; refused=$((refused + 1)); break; fi
         echo "delivery API answered $code for $name; retrying"
         if [ "$attempt" -lt 6 ]; then sleep $((1 << attempt)); else sleep 60; fi ;;
    esac
  done
done
echo "delivered $delivered document(s) to $url, $refused refused"
[ "$refused" -eq 0 ]
`

// renderDeliveryJob writes the Job delivering documents to the host's delivery
// route into the overlay directory, and returns the manifest's file name.
func renderDeliveryJob(directory, name, namespace, serviceAccount, kind, path string, target *DeliveryTarget, documents []deliveryDocument) (string, error) {
	if target == nil || target.URL == "" {
		return "", fmt.Errorf("the environment names no delivery endpoint, so nothing can deliver the %s documents it renders", kind)
	}
	if len(documents) == 0 {
		return "", fmt.Errorf("no %s document to deliver", kind)
	}
	sorted := append([]deliveryDocument(nil), documents...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	sources := make([]deliveryProjectedSource, 0, len(sorted))
	for _, document := range sorted {
		sources = append(sources, deliveryProjectedSource{ConfigMap: &deliveryConfigMapSource{
			Name:  document.ConfigMap,
			Items: []deliveryKeyToPath{{Key: document.Key, Path: document.Name}},
		}})
	}
	labels := map[string]string{managedByLabel: managedByCodefly, deliveryLabel: kind}
	job := deliveryJobManifest{
		APIVersion: "batch/v1",
		Kind:       kindJob,
		Metadata: deliveryJobMetadata{
			Name: name, Namespace: namespace, Labels: labels,
			Annotations: map[string]string{
				argoHookAnnotation:     argoHookSync,
				argoHookDeletePolicy:   argoHookBeforeHookCreation,
				argoSyncWaveAnnotation: deliveryWave,
			},
		},
		Spec: deliveryJobSpec{BackoffLimit: deliveryBackoffLimit, ActiveDeadlineSeconds: deliveryDeadlineSeconds},
	}
	job.Spec.Template.Metadata.Labels = labels
	job.Spec.Template.Spec = deliveryPodSpec{
		ServiceAccountName: serviceAccount,
		// The default mount is the unscoped API token; the only token this pod
		// needs is the one projected for the host's audience below.
		AutomountServiceAccountToken: false,
		RestartPolicy:                "OnFailure",
		SecurityContext: map[string]any{
			"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		Containers: []deliveryContainer{{
			Name:    deliveryContainerName,
			Image:   deliveryImage,
			Command: []string{"/bin/sh", "-ec", deliveryScript},
			Env: []deliveryEnv{
				{Name: "DELIVERY_URL", Value: target.URL},
				{Name: "DELIVERY_PATH", Value: path},
				{Name: "DELIVERY_DOCUMENTS", Value: deliveryDocumentsMount},
				// Named for what it is, the FILE the projected identity is read
				// from: a variable named *_TOKEN carrying a value is classified
				// as a credential by the promotable ruleset, which refuses the
				// manifest — and the value is a path, never the token.
				{Name: "DELIVERY_IDENTITY_FILE", Value: deliveryTokenMount + "/" + deliveryTokenFile},
			},
			VolumeMounts: []deliveryVolumeMount{
				{Name: "documents", MountPath: deliveryDocumentsMount, ReadOnly: true},
				{Name: "token", MountPath: deliveryTokenMount, ReadOnly: true},
			},
			Resources: deliveryResources{
				Requests: deliveryAmounts{CPU: "10m", Memory: "32Mi"},
				Limits:   deliveryAmounts{CPU: "100m", Memory: "64Mi"},
			},
			SecurityContext: map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"capabilities":             map[string]any{"drop": []string{"ALL"}},
			},
		}},
		Volumes: []deliveryVolume{
			{Name: "documents", Projected: deliveryProjected{Sources: sources}},
			{Name: "token", Projected: deliveryProjected{Sources: []deliveryProjectedSource{{
				ServiceAccountToken: &deliveryTokenSource{Audience: target.Audience, ExpirationSeconds: deliveryTokenSeconds, Path: deliveryTokenFile},
			}}}},
		},
	}
	body, err := yaml.Marshal(job)
	if err != nil {
		return "", fmt.Errorf("encode %s delivery job: %w", kind, err)
	}
	file := "deliver-" + kind + ".yaml"
	if err := os.WriteFile(filepath.Join(directory, file), body, 0o644); err != nil { //nolint:gosec // a delivered manifest, readable beside the rest of the tree
		return "", fmt.Errorf("write %s delivery job: %w", kind, err)
	}
	return file, nil
}

// renderDeliveryServiceAccount writes the ServiceAccount the module's delivery
// Job runs as, into its namespace, and returns the manifest's file name.
func renderDeliveryServiceAccount(directory, namespace string) (string, error) {
	account := deliveryServiceAccountManifest{
		APIVersion: "v1",
		Kind:       kindServiceAccount,
		Metadata: deliveryAccountMetadata{
			Name: deliveryServiceAccount, Namespace: namespace,
			Labels: map[string]string{managedByLabel: managedByCodefly, deliveryLabel: deliveryPresence},
		},
		AutomountServiceAccountToken: false,
	}
	body, err := yaml.Marshal(account)
	if err != nil {
		return "", fmt.Errorf("encode delivery service account: %w", err)
	}
	const file = "delivery-service-account.yaml"
	if err := os.WriteFile(filepath.Join(directory, file), body, 0o644); err != nil { //nolint:gosec // a delivered manifest, readable beside the rest of the tree
		return "", fmt.Errorf("write delivery service account: %w", err)
	}
	return file, nil
}

// deliveryJobName is the Job's object name: one per document type per module,
// bounded to a Kubernetes name.
// deliveryJobName names the Job that delivers one settled set: the kind, the
// module, the environment and a digest of the carriers it posts. The digest
// makes the Job per generation — a changed set is a new Job, which the hook's
// BeforeHookCreation policy replaces, and an unchanged set is the same Job,
// which a re-sync leaves alone. The environment keeps two environments of one
// workspace on one cluster from replacing each other's Job in the authority
// namespace, which they share.
func deliveryJobName(kind, module, environment string, carriers [][]byte) string {
	sum := sha256.New()
	for _, carrier := range carriers {
		sum.Write(carrier)
		sum.Write([]byte{0})
	}
	return argoBoundedName(63, "deliver", kind, strings.ToLower(module), strings.ToLower(environment), hex.EncodeToString(sum.Sum(nil))[:10])
}
