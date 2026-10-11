package localcluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
)

// TrustAnchorAnnotation is the pod-template annotation that records which
// trust anchor a verifier's pods were started with. The render refuses a
// workload that carries it in desired state (gitops.TrustAnchorAnnotation), so
// Argo CD never owns a value that would revert the stamp.
const TrustAnchorAnnotation = gitops.TrustAnchorAnnotation

// AnchorDigest is the digest of both trust-anchor objects' contents, by file.
func AnchorDigest(policy, trustedRoot []byte) string {
	hash := sha256.New()
	for _, part := range [][]byte{[]byte(VerificationPolicyFile), policy, []byte(TrustedRootFile), trustedRoot} {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		hash.Write(part)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// anchorWorkload is the part of a Deployment, StatefulSet or DaemonSet the
// reconciliation reads.
type anchorWorkload struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Selector struct {
			MatchLabels      map[string]string `json:"matchLabels"`
			MatchExpressions []json.RawMessage `json:"matchExpressions"`
		} `json:"selector"`
		UpdateStrategy struct {
			Type          string `json:"type"`
			RollingUpdate struct {
				Partition *int `json:"partition"`
			} `json:"rollingUpdate"`
		} `json:"updateStrategy"`
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec map[string]any `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func (workload *anchorWorkload) reference() string {
	return strings.ToLower(workload.Kind) + "/" + workload.Metadata.Name
}

// consumesAnchor reports whether a pod template reads either trust-anchor
// object by any means — a configMap or secret volume, a projected source,
// envFrom, or an env valueFrom — whatever the volume or variable is named.
func consumesAnchor(spec map[string]any) bool {
	names := func(value any) bool {
		object, _ := value.(map[string]any)
		name, _ := object["name"].(string)
		secretName, _ := object["secretName"].(string)
		return name == gitops.LocalTrustAnchorObjects || secretName == gitops.LocalTrustAnchorObjects
	}
	for _, raw := range asList(spec["volumes"]) {
		volume, _ := raw.(map[string]any)
		if names(volume["configMap"]) || names(volume["secret"]) {
			return true
		}
		projected, _ := volume["projected"].(map[string]any)
		for _, source := range asList(projected["sources"]) {
			entry, _ := source.(map[string]any)
			if names(entry["configMap"]) || names(entry["secret"]) {
				return true
			}
		}
	}
	for _, kind := range []string{"containers", "initContainers"} {
		for _, raw := range asList(spec[kind]) {
			container, _ := raw.(map[string]any)
			for _, from := range asList(container["envFrom"]) {
				entry, _ := from.(map[string]any)
				if names(entry["configMapRef"]) || names(entry["secretRef"]) {
					return true
				}
			}
			for _, variable := range asList(container["env"]) {
				entry, _ := variable.(map[string]any)
				valueFrom, _ := entry["valueFrom"].(map[string]any)
				if names(valueFrom["configMapKeyRef"]) || names(valueFrom["secretKeyRef"]) {
					return true
				}
			}
		}
	}
	return false
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

// reconcileTrustAnchor brings every Deployment, StatefulSet and DaemonSet in
// the namespace that reads the local trust anchor onto its current digest,
// and proves it: the digest is stamped on the pod template, the rollout is
// awaited, the template is read back (a competing rollout must not have
// reverted it), and EVERY live pod of the workload must carry the digest —
// a pod started on the old anchor still enforces the old policy, a removed
// developer included. An update strategy that keeps old pods by design
// (OnDelete, a partitioned StatefulSet) cannot be proven and is refused.
// It is decided by the cluster's state, never by what an apply reported, so
// whatever failed is retried by the next run.
func (d *ClusterDriver) reconcileTrustAnchor(ctx context.Context, namespace, digest string) error {
	listed, err := d.Kubectl(ctx, "", "-n", namespace, "get", "deployments,statefulsets,daemonsets", "-o", "json")
	if err != nil {
		return fmt.Errorf("list the workloads reading the local trust anchor in %s: %w", namespace, err)
	}
	var workloads struct {
		Items []anchorWorkload `json:"items"`
	}
	if err = json.Unmarshal([]byte(listed), &workloads); err != nil {
		return fmt.Errorf("read the workloads of %s: %w", namespace, err)
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{keyMetadata: map[string]any{
		"annotations": map[string]string{TrustAnchorAnnotation: digest},
	}}}})
	if err != nil {
		return err
	}
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		if !consumesAnchor(workload.Spec.Template.Spec) {
			continue
		}
		reference := workload.reference()
		if workload.Spec.Template.Metadata.Annotations[TrustAnchorAnnotation] != digest {
			if _, err = d.Kubectl(ctx, "", "-n", namespace, "patch", reference, "--type", "merge", "-p", string(patch)); err != nil {
				return fmt.Errorf("stamp the trust anchor %s on %s (the next run retries): %w", digest, reference, err)
			}
		}
		if _, err = d.Kubectl(ctx, "", "-n", namespace, "rollout", "status", reference, "--timeout=5m"); err != nil {
			return fmt.Errorf("%s has not rolled out on the trust anchor %s (the next run waits again): %w", reference, digest, err)
		}
		if err = d.proveAnchorReached(ctx, namespace, workload, digest); err != nil {
			return err
		}
	}
	return nil
}

// proveAnchorReached waits until the workload's template still carries the
// digest and every live pod it selects was started with it.
func (d *ClusterDriver) proveAnchorReached(ctx context.Context, namespace string, workload *anchorWorkload, digest string) error {
	reference := workload.reference()
	if len(workload.Spec.Selector.MatchExpressions) > 0 || len(workload.Spec.Selector.MatchLabels) == 0 {
		return fmt.Errorf("cannot prove the trust anchor reached every pod of %s: its selector is not a plain label match", reference)
	}
	keys := make([]string, 0, len(workload.Spec.Selector.MatchLabels))
	for key := range workload.Spec.Selector.MatchLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	selector := make([]string, 0, len(keys))
	for _, key := range keys {
		selector = append(selector, key+"="+workload.Spec.Selector.MatchLabels[key])
	}
	deadline := time.Now().Add(d.rolloutTimeout())
	for {
		current, err := d.Kubectl(ctx, "", "-n", namespace, "get", reference, "-o", "json")
		if err != nil {
			return fmt.Errorf("read %s back: %w", reference, err)
		}
		var now anchorWorkload
		if err = json.Unmarshal([]byte(current), &now); err != nil {
			return fmt.Errorf("read %s back: %w", reference, err)
		}
		if got := now.Spec.Template.Metadata.Annotations[TrustAnchorAnnotation]; got != digest {
			return fmt.Errorf("%s was moved off the trust anchor %s (it carries %q now) by another rollout; the next run stamps it again", reference, digest, got)
		}
		pods, err := d.Kubectl(ctx, "", "-n", namespace, "get", "pods", "-l", strings.Join(selector, ","), "-o", "json")
		if err != nil {
			return fmt.Errorf("list the pods of %s: %w", reference, err)
		}
		var listed struct {
			Items []struct {
				Metadata struct {
					Name        string            `json:"name"`
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err = json.Unmarshal([]byte(pods), &listed); err != nil {
			return fmt.Errorf("list the pods of %s: %w", reference, err)
		}
		var stale []string
		for _, pod := range listed.Items {
			if pod.Metadata.Annotations[TrustAnchorAnnotation] != digest {
				stale = append(stale, pod.Metadata.Name)
			}
		}
		if len(stale) == 0 {
			return nil
		}
		if keepsOldPods(&now) {
			return fmt.Errorf("cannot prove the trust anchor reached every pod of %s: its update strategy keeps pods on their old template (OnDelete, or a partitioned StatefulSet), and %s still run the previous anchor; delete them or change the strategy", reference, strings.Join(stale, ", "))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pods %s of %s still run the previous trust anchor after %s; the next run waits again", strings.Join(stale, ", "), reference, d.rolloutTimeout())
		}
		if err = d.pause(ctx); err != nil {
			return err
		}
	}
}

// keepsOldPods reports an update strategy under which a changed template does
// not replace the running pods.
func keepsOldPods(workload *anchorWorkload) bool {
	strategy := &workload.Spec.UpdateStrategy
	if strategy.Type == "OnDelete" {
		return true
	}
	return workload.Kind == "StatefulSet" && strategy.RollingUpdate.Partition != nil && *strategy.RollingUpdate.Partition > 0
}

func (d *ClusterDriver) rolloutTimeout() time.Duration {
	if d.RolloutTimeout > 0 {
		return d.RolloutTimeout
	}
	return 5 * time.Minute
}
