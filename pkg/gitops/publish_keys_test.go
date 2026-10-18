package gitops

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/solutionhost"
)

func TestDeveloperAdmissionPrecedesSigning(t *testing.T) {
	signer := &fakeSigner{}
	env := &environments.Environment{Name: "local", Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-dev"}, Host: &environments.EnvironmentHost{Domain: "example", Developers: []environments.HostDeveloper{{KeyID: "dev-ffffffffffffffff"}}}}
	publication := &deliveryPublication{local: true, supplied: true, options: deliveryPublishOptions{Signer: signer}}
	err := publication.signAsDeveloper(env)
	want := "your developer key is " + signer.KeyID() + "; add `- key_id: " + signer.KeyID() + "` under host.developers of environment local"
	if err == nil || err.Error() != want || signer.signed != 0 {
		t.Fatalf("preflight refusal: %v, signatures %d", err, signer.signed)
	}
	env.Host.Developers[0].KeyID = signer.KeyID()
	if err = publication.signAsDeveloper(env); err != nil {
		t.Fatal(err)
	}
	if publication.options.AllowUnsigned || publication.options.SelfCheck == nil || publication.options.ReuseCheck == nil {
		t.Fatal("developer checks missing")
	}
}

func TestReleaseAdmissionChecksSuppliedCustodyAndReusedCarriers(t *testing.T) {
	signer := (&fakeSigner{}).custody()
	env := &environments.Environment{Name: "production", Host: &environments.EnvironmentHost{Domain: "example", Release: &environments.HostReleasePolicy{KeyID: "different-v1"}}}
	publication := &deliveryPublication{options: deliveryPublishOptions{Signer: signer}}
	if err := publication.holdToReleasePolicy(env); err == nil || !strings.Contains(err.Error(), signer.KeyID()) || !strings.Contains(err.Error(), "different-v1") {
		t.Fatalf("custody mismatch: %v", err)
	}
	env.Host.Release.KeyID = signer.KeyID()
	if err := publication.holdToReleasePolicy(env); err != nil {
		t.Fatal(err)
	}
	manifest, err := ModuleManifestOf("example.prod.crm", testHost(), solutionRenderOptions("").Instance,
		[]solutionhost.Workload{{Service: "api", Container: "api"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := solutionhost.SignedPayload(manifest)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := signing.SignEnvelope(payload, signer)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []signing.SelfCheck{publication.options.SelfCheck, publication.options.ReuseCheck} {
		if err = check(context.Background(), envelope, payload); err != nil {
			t.Fatal(err)
		}
	}
	env.Host.Domain = "other"
	if err = publication.holdToReleasePolicy(env); err != nil {
		t.Fatal(err)
	}
	if err = publication.options.ReuseCheck(context.Background(), envelope, payload); err == nil {
		t.Fatal("reuse bypassed changed ownership domain")
	}
}

func TestLocalQualificationNeverUsesASuppliedReleaseKey(t *testing.T) {
	publication := &deliveryPublication{local: true, supplied: true, options: deliveryPublishOptions{Signer: (&fakeSigner{}).custody()}}
	env := &environments.Environment{Name: "local", Host: &environments.EnvironmentHost{}}
	if err := publication.signAsDeveloper(env); err != nil {
		t.Fatal(err)
	}
	if _, ok := publication.options.Signer.(*signing.Unavailable); !ok || !publication.options.AllowUnsigned {
		t.Fatal("qualification signed with release custody")
	}
}
