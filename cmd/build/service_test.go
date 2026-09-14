package build

import (
	"path/filepath"
	"testing"
)

func TestServiceCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("build service command is not exclusively RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("build service accepted two service names")
	}
}

// Collecting evidence runs a container scanner and fails a build whose agent
// cannot serve image scope, which no released agent does yet, so the flag has to
// stay off until a caller asks for it.
func TestServiceCommandKeepsImageEvidenceOptIn(t *testing.T) {
	flag := ServiceCmd.Flags().Lookup("image-sbom")
	if flag == nil {
		t.Fatal("build service does not accept --image-sbom")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--image-sbom defaults to %q, want off", flag.DefValue)
	}
	directory := ServiceCmd.Flags().Lookup("image-sbom-dir")
	if directory == nil {
		t.Fatal("build service does not accept --image-sbom-dir")
	}
	if want := filepath.Join(".codefly", "sbom", "image"); directory.DefValue != want {
		t.Fatalf("--image-sbom-dir defaults to %q, want %q", directory.DefValue, want)
	}
}

// The publisher owns the opt-in gate, so a build that never asked for evidence
// publishes nothing however it is reached.
func TestPublishImageEvidenceIsANoOpWhenNotRequested(t *testing.T) {
	imageSBOM = false
	if err := publishImageEvidence(nil, nil); err != nil {
		t.Fatalf("publishing without --image-sbom returned %v", err)
	}
}

func TestModuleCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ModuleCmd.RunE == nil || ModuleCmd.Run != nil {
		t.Fatal("build module command is not exclusively RunE")
	}
	if err := ModuleCmd.Args(ModuleCmd, []string{"one", "two"}); err == nil {
		t.Fatal("build module accepted two module names")
	}
}
