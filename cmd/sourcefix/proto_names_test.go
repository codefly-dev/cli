package sourcefix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A service as the go-grpc scaffold leaves it, moved to its own names.
func TestProtoNamesMovesAScaffoldedContract(t *testing.T) {
	dir := t.TempDir()
	writeFixFile(t, filepath.Join(dir, "proto", "api.proto"),
		"syntax = \"proto3\";\npackage api;\n\noption go_package = \"calls/pkg/gen;gen\";\n\nmessage VersionRequest {}\n")
	writeFixFile(t, filepath.Join(dir, "code", "pkg", "adapters", "service.go"),
		"package adapters\n\nimport (\n\t\"context\"\n\n\t\"calls/pkg/gen\"\n\tgenconnect \"calls/pkg/gen/genconnect\"\n\t\"calls/pkg/general\"\n)\n\nvar _ = gen.VersionRequest{}\n")
	writeFixFile(t, filepath.Join(dir, "code", "main.go"), "package main\n\nimport pb \"calls/pkg/gen\"\n\nvar _ pb.VersionRequest\n")
	writeFixFile(t, filepath.Join(dir, "code", "pkg", "adapters", "grpc_gen.go"), "package adapters\n\nimport gen \"calls/pkg/gen\"\n")
	writeFixFile(t, filepath.Join(dir, "code", "pkg", "gen", "api.pb.go"), "package gen\n\nimport _ \"calls/pkg/gen/other\"\n")

	plan, err := PlanProtoNames(dir, "phone", "calls")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Package != "phone.calls.v1" || plan.To != filepath.Join("proto", "phone", "calls", "v1", "api.proto") || plan.GoImport != "calls/pkg/gen/phone/calls/v1" {
		t.Fatalf("plan: %+v", plan)
	}
	// Its own code, not what a generator owns.
	if got := strings.Join(plan.GoFiles, ","); got != filepath.Join("code", "main.go")+","+filepath.Join("code", "pkg", "adapters", "service.go") {
		t.Fatalf("files to rewrite: %s", got)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "proto", "api.proto")); !os.IsNotExist(err) {
		t.Fatal("the old contract is gone")
	}
	moved, _ := os.ReadFile(filepath.Join(dir, plan.To))
	if !strings.Contains(string(moved), "package phone.calls.v1;") || !strings.Contains(string(moved), `go_package = "calls/pkg/gen/phone/calls/v1;gen";`) {
		t.Fatalf("moved contract:\n%s", moved)
	}
	service, _ := os.ReadFile(filepath.Join(dir, "code", "pkg", "adapters", "service.go"))
	for _, want := range []string{"\tgen \"calls/pkg/gen/phone/calls/v1\"\n", "\tgenconnect \"calls/pkg/gen/phone/calls/v1/genconnect\"\n", "\t\"calls/pkg/general\"\n"} {
		if !strings.Contains(string(service), want) {
			t.Errorf("service.go misses %q:\n%s", want, service)
		}
	}
	main, _ := os.ReadFile(filepath.Join(dir, "code", "main.go"))
	if !strings.Contains(string(main), "import pb \"calls/pkg/gen/phone/calls/v1\"\n") {
		t.Errorf("an import that has a name keeps it:\n%s", main)
	}
	// Done once: there is nothing left in the scaffold's layout.
	if _, err := PlanProtoNames(dir, "phone", "calls"); err == nil || !strings.Contains(err.Error(), "no proto/api.proto") {
		t.Fatalf("a moved service is refused: %v", err)
	}
}

func TestProtoNamesPackage(t *testing.T) {
	dir := t.TempDir()
	writeFixFile(t, filepath.Join(dir, "proto", "api.proto"), "syntax = \"proto3\";\npackage api;\n")
	// Module and service sharing a name are not said twice; no go_package,
	// no Go to rewrite.
	plan, err := PlanProtoNames(dir, "shop", "shop")
	if err != nil || plan.Package != "shop.v1" || plan.GoImport != "" || len(plan.Describe()) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if got := protoIdent("My-Service 2"); got != "my_service_2" {
		t.Fatalf("ident: %s", got)
	}
	if got := protoIdent("2fast"); got != "s2fast" {
		t.Fatalf("a package element cannot start with a digit: %s", got)
	}
}
