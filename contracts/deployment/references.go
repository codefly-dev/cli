package deployment

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"
)

func validateReferences(inv Inventory, p profiles) error {
	members := map[string]Member{}
	artifacts := map[string]Artifact{}
	namespaces := map[string]Namespace{}
	workloads := map[string]Workload{}
	objects := map[ObjectIdentity]string{}
	for _, m := range inv.Members {
		if !idPattern.MatchString(m.Binding) || members[m.Binding].Binding != "" || m.PresenceGeneration < 1 || !slices.Contains([]string{"module", "solution"}, m.DeclarationKind) || !digestPattern.MatchString(m.Digest) || !unique(m.Artifacts) || !unique(m.Workloads) {
			return refuse("MEMBER", "$/members", "invalid or duplicate member binding, digest, generation or references")
		}
		if m.Removed && (len(m.Artifacts) > 0 || len(m.Workloads) > 0) {
			return refuse("REMOVAL", "$/members", "removed member cannot retain execution references")
		}
		members[m.Binding] = m
	}
	sources := map[string]bool{}
	for _, s := range p.execution.Sources {
		sources[s.ID] = true
	}
	for _, a := range inv.Artifacts {
		if !idPattern.MatchString(a.ID) || artifacts[a.ID].ID != "" || !digestPattern.MatchString(a.Digest) {
			return refuse("ARTIFACT", "$/artifacts", "invalid or duplicate artifact identity")
		}
		if !sources[a.Source] || !cleanRelative(a.Path) {
			return refuse("ARTIFACT_SOURCE", "$/artifacts", "artifact path must be confined to a context-approved source")
		}
		for _, o := range a.Objects {
			if !slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "ConfigMap", "Secret", "Service"}, o.Kind) {
				return refuse("ARTIFACT_OBJECT_KIND", "$/artifacts", "unsupported artifact object kind")
			}
			if slices.Contains([]string{"ConfigMap", "Secret", "Service"}, o.Kind) && o.APIVersion != "v1" {
				return refuse("ARTIFACT_OBJECT_KIND", "$/artifacts", "unsupported artifact API version")
			}
			if objects[o] != "" {
				return refuse("OBJECT_UNIQUE", "$/artifacts", "duplicate artifact object identity")
			}
			if !dnsSubdomain(o.Name) || !dnsName(o.Namespace) || o.APIVersion == "" || o.Kind == "" {
				return refuse("OBJECT_IDENTITY", "$/artifacts", "invalid artifact object identity")
			}
			objects[o] = a.ID
		}
		artifacts[a.ID] = a
	}
	for _, n := range inv.Namespaces {
		m, ok := members[n.MemberBinding]
		if !ok || m.Removed || !dnsName(n.Name) || namespaces[n.Name].Name != "" {
			return refuse("NAMESPACE", "$/namespaces", "namespace must have one active owner")
		}
		namespaces[n.Name] = n
	}
	for _, w := range inv.Workloads {
		workloads[w.ID] = w
	}
	catalog := map[string]CatalogEntry{}
	for _, c := range p.execution.Catalog {
		catalog[c.ID] = c
	}
	artifactOwners := map[string]string{}
	workloadOwners := map[string]string{}
	for _, m := range inv.Members {
		for _, id := range m.Artifacts {
			if artifacts[id].ID == "" || artifactOwners[id] != "" {
				return referenceError("$/members", "artifact ownership")
			}
			artifactOwners[id] = m.Binding
		}
		for _, id := range m.Workloads {
			if workloads[id].ID == "" || workloadOwners[id] != "" {
				return referenceError("$/members", "workload ownership")
			}
			workloadOwners[id] = m.Binding
		}
	}
	for _, a := range inv.Artifacts {
		if artifactOwners[a.ID] == "" {
			return referenceError("$/artifacts", "artifact member")
		}
		for _, o := range a.Objects {
			n, ok := namespaces[o.Namespace]
			if !ok || n.MemberBinding != artifactOwners[a.ID] {
				return referenceError("$/artifacts", "object namespace ownership")
			}
			// Every supported controller declared in an artifact has exactly one contract.
			if slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}, o.Kind) {
				found := false
				for _, w := range inv.Workloads {
					if w.Controller == o {
						found = true
					}
				}
				if !found {
					return referenceError("$/artifacts", "Pod-producing object contract")
				}
			}
		}
	}
	principals := map[string]bool{}
	for _, w := range inv.Workloads {
		wp := "$/workloads/" + w.ID
		m, ok := members[w.MemberBinding]
		n, nok := namespaces[w.Controller.Namespace]
		if !ok || m.Removed || workloadOwners[w.ID] != w.MemberBinding || !nok || n.MemberBinding != w.MemberBinding {
			return referenceError(wp, "workload ownership or namespace")
		}
		if !slices.Contains(p.compiler.SupportedKinds, w.Controller.Kind) {
			return refuse("CONTROLLER_PROFILE", wp, "kind is not enabled by referenced compiler profile")
		}
		if !slices.Contains(p.execution.CredentialKinds, w.CredentialKind) {
			return refuse("CREDENTIAL_PROFILE", wp, "credential kind is not enabled by execution profile")
		}
		if w.Artifact.Artifact != nil {
			a := *w.Artifact.Artifact
			if artifactOwners[a] != w.MemberBinding || objects[w.Controller] != a {
				return referenceError(wp, "workload artifact/controller")
			}
		} else {
			c, err := catalogEntry(catalog, *w.Artifact.CarrierProfile, "carrier", w)
			if err != nil {
				return err
			}
			raw, err := marshalCanonical(w.Template)
			if err != nil {
				return err
			}
			if Digest(raw) != c.Digest {
				return refuse("CARRIER_TEMPLATE", wp, "carrier template does not match retained catalogue contract")
			}
			if w.PlatformRoleRef == nil {
				return referenceError(wp, "carrier role")
			}
		}
		if w.PlatformRoleRef != nil {
			if _, err := catalogEntry(catalog, *w.PlatformRoleRef, "role", w); err != nil {
				return err
			}
		}
		if w.CredentialKind == "platform" && w.PlatformRoleRef == nil {
			return referenceError(wp, "platform role")
		}
		if w.Identity != nil {
			principal := w.Controller.Namespace + "\x00" + w.Template.Spec.ServiceAccountName
			if principals[principal] {
				return refuse("PRINCIPAL_UNIQUE", wp, "credential-bearing executions require distinct service-account principals")
			}
			principals[principal] = true
			match := false
			for _, g := range p.identity.Grants {
				if g.Workload == w.ID && g.Namespace == w.Controller.Namespace && g.ServiceAccount == w.Template.Spec.ServiceAccountName && g.CredentialKind == w.CredentialKind && g.Container == *w.AuthenticatingContainer && reflect.DeepEqual(g.Identity, *w.Identity) {
					match = true
				}
			}
			if !match {
				return refuse("IDENTITY_GRANT", wp, "identity must match an independent scoped grant exactly")
			}
			id := w.Identity
			if !strings.HasPrefix(id.Issuer, "https://") || id.Audience == "" || id.Subject != "system:serviceaccount:"+w.Controller.Namespace+":"+w.Template.Spec.ServiceAccountName || !strings.HasPrefix(id.SPIFFEID, "spiffe://") || !unique(id.Attachments) {
				return refuse("IDENTITY_VALUE", wp, "invalid token/SPIFFE identity or attachment references")
			}
			for _, a := range id.Attachments {
				if _, err := catalogEntry(catalog, a, "attachment", w); err != nil {
					return err
				}
			}
		}
		if err := configuration(w, catalog); err != nil {
			return err
		}
	}
	if inv.Removed {
		if len(inv.Workloads) > 0 || len(inv.Artifacts) > 0 || len(inv.Namespaces) > 0 || len(inv.Dependencies) > 0 || len(inv.Ingress) > 0 || len(inv.Egress) > 0 || len(inv.ResourceBindings) > 0 {
			return refuse("REMOVAL", "$/removed", "withdrawn delivery must have no active execution/resources")
		}
		for _, m := range inv.Members {
			if !m.Removed {
				return refuse("REMOVAL", "$/members", "withdrawn delivery requires explicit member removal")
			}
		}
	}
	for _, d := range inv.Dependencies {
		consumer, ok := workloads[d.Consumer]
		if !ok || !validPort(d.Protocol, d.Port) {
			return referenceError("$/dependencies", "consumer or endpoint")
		}
		if d.Provider.Workload != nil {
			provider, ok := workloads[*d.Provider.Workload]
			if !ok || !endpoint(provider, d.Provider.Endpoint, d.Protocol, d.Port) {
				return referenceError("$/dependencies", "provider endpoint")
			}
		} else {
			e, err := catalogEntry(catalog, d.Provider.Endpoint, "endpoint", consumer)
			if err != nil {
				return err
			}
			if e.Protocol != d.Protocol || e.Port != d.Port {
				return referenceError("$/dependencies", "catalogue endpoint protocol/port")
			}
		}
	}
	// No qualified ingress/CIDR/resource enforcement profile ships with v1. Refuse
	// requests rather than carrying constraints that a compiler could omit.
	if len(inv.Ingress) > 0 {
		return refuse("INGRESS_UNSUPPORTED", "$/ingress", "ingress enforcement is not qualified by explicit-v1")
	}
	for _, e := range inv.Egress {
		if len(e.CIDRs) > 0 {
			return refuse("CIDR_UNSUPPORTED", "$/egress", "CIDR enforcement is not qualified by explicit-v1")
		}
		if !dnsSubdomain(e.Host) || !validPort(e.Protocol, e.Port) || len(e.Consumers) == 0 || !unique(e.Consumers) {
			return refuse("EGRESS", "$/egress", "invalid egress destination or consumer set")
		}
		for _, id := range e.Consumers {
			if workloads[id].ID == "" {
				return referenceError("$/egress", "consumer")
			}
		}
	}
	if len(inv.ResourceBindings) > 0 {
		return refuse("RESOURCE_UNSUPPORTED", "$/resource_bindings", "resource enforcement is not qualified by explicit-v1")
	}
	return nil
}
func catalogEntry(catalog map[string]CatalogEntry, id, kind string, w Workload) (CatalogEntry, error) {
	e, ok := catalog[id]
	if !ok || e.Kind != kind || e.Namespace != w.Controller.Namespace || !slices.Contains(e.Workloads, w.ID) || !slices.Contains(e.CredentialKinds, w.CredentialKind) {
		return e, referenceError("$/workloads/"+w.ID, "scoped catalogue "+kind)
	}
	return e, nil
}
func endpoint(w Workload, name, protocol string, number int64) bool {
	for _, c := range w.Template.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == name && p.Protocol == protocol && p.ContainerPort == number {
				return true
			}
		}
	}
	return false
}
func configuration(w Workload, catalog map[string]CatalogEntry) error {
	p := "$/workloads/" + w.ID + "/configuration_refs"
	declared := map[string]bool{}
	actual := map[string]bool{}
	refs := map[string]ConfigurationRef{}
	ids := map[string]bool{}
	useKey := func(kind, name, container, use, at, key string) string {
		return strings.Join([]string{kind, name, container, use, at, key}, "\x00")
	}
	for _, r := range w.ConfigurationRefs {
		if !idPattern.MatchString(r.ID) || ids[r.ID] || !slices.Contains([]string{"configmap", "secret"}, r.Kind) || !dnsSubdomain(r.Name) || len(r.Consumers) == 0 {
			return refuse("CONFIGURATION_REF", p, "invalid or duplicate configuration reference")
		}
		ids[r.ID] = true
		entry, err := catalogEntry(catalog, r.CatalogRef, r.Kind, w)
		if err != nil {
			return err
		}
		if entry.Name != r.Name || entry.Version != r.Version {
			return referenceError(p, "configuration name/version")
		}
		key := r.Kind + "\x00" + r.Name
		if _, ok := refs[key]; ok {
			return refuse("CONFIGURATION_REF", p, "duplicate configuration source")
		}
		refs[key] = r
		for _, u := range r.Consumers {
			k := useKey(r.Kind, r.Name, u.Container, u.Use, u.Name, u.Key)
			if declared[k] {
				return refuse("CONFIGURATION_REF", p, "duplicate consumer use")
			}
			declared[k] = true
		}
	}
	volumes := map[string]Volume{}
	for _, v := range w.Template.Spec.Volumes {
		if !dnsName(v.Name) || volumes[v.Name].Name != "" || (v.ConfigMap == nil) == (v.Secret == nil) {
			return refuse("CONFIGURATION_USE", p, "invalid volume or ambiguous source")
		}
		volumes[v.Name] = v
		var name string
		var mode int64
		var optional bool
		var items []KeyToPath
		if v.ConfigMap != nil {
			name = v.ConfigMap.Name
			mode = v.ConfigMap.DefaultMode
			optional = v.ConfigMap.Optional
			items = v.ConfigMap.Items
		} else {
			name = v.Secret.SecretName
			mode = v.Secret.DefaultMode
			optional = v.Secret.Optional
			items = v.Secret.Items
		}
		if !dnsSubdomain(name) || optional || mode < 0 || mode > 511 || len(items) == 0 {
			return refuse("CONFIGURATION_USE", p, "volume must explicitly enumerate nonoptional keys and valid modes")
		}
		keys := map[string]bool{}
		paths := map[string]bool{}
		for _, item := range items {
			if item.Key == "" || keys[item.Key] || paths[item.Path] || !cleanRelative(item.Path) || item.Mode < 0 || item.Mode > 511 {
				return refuse("CONFIGURATION_USE", p, "invalid or duplicate volume item")
			}
			keys[item.Key] = true
			paths[item.Path] = true
		}
	}
	mounted := map[string]bool{}
	for _, containers := range [][]Container{w.Template.Spec.Containers, w.Template.Spec.InitContainers} {
		for _, c := range containers {
			for _, env := range c.Env {
				if env.ValueFrom == nil {
					continue
				}
				kind := "configmap"
				key := env.ValueFrom.ConfigMapKeyRef
				if key == nil {
					kind = "secret"
					key = env.ValueFrom.SecretKeyRef
				}
				actual[useKey(kind, key.Name, c.Name, "env", env.Name, key.Key)] = true
			}
			mountPaths := map[string]bool{}
			mountNames := map[string]bool{}
			for _, mount := range c.VolumeMounts {
				v, ok := volumes[mount.Name]
				if !ok || !mount.ReadOnly || mountPaths[mount.MountPath] || mountNames[mount.Name] || !strings.HasPrefix(mount.MountPath, "/") || path.Clean(mount.MountPath) != mount.MountPath {
					return refuse("CONFIGURATION_USE", p, "invalid, duplicate or writable mount")
				}
				mountPaths[mount.MountPath] = true
				mountNames[mount.Name] = true
				mounted[mount.Name] = true
				kind := "configmap"
				name := ""
				var items []KeyToPath
				if v.ConfigMap != nil {
					name = v.ConfigMap.Name
					items = v.ConfigMap.Items
				} else {
					kind = "secret"
					name = v.Secret.SecretName
					items = v.Secret.Items
				}
				for _, item := range items {
					actual[useKey(kind, name, c.Name, "mount", mount.MountPath, item.Key)] = true
				}
			}
		}
	}
	if len(mounted) != len(volumes) {
		return refuse("CONFIGURATION_USE", p, "unused volume is not an approved consumer use")
	}
	if !reflect.DeepEqual(actual, declared) {
		return refuse("CONFIGURATION_CLOSURE", p, fmt.Sprintf("declared configuration consumers must exactly equal template uses (%d declared, %d used)", len(declared), len(actual)))
	}
	return nil
}
