package gateway

import (
	"reflect"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// TestGatewayInspectsJVMAndDotNetCodeUnitsThroughGenericAgent proves the
// production Gateway binds each declared unit to Codefly's real generic agent
// and returns repository-relative typed evidence. No project manifest or
// source file crosses into the caller for local interpretation.
func TestGatewayInspectsJVMAndDotNetCodeUnitsThroughGenericAgent(t *testing.T) {
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "src/ads/settings.gradle", "rootProject.name = 'adservice'\n")
	writeCodeUnitFixture(t, root, "src/ads/build.gradle", `
group = "example.ads"
def grpcVersion = "1.82.1"
sourceCompatibility = JavaVersion.VERSION_21
dependencies {
    implementation "io.grpc:grpc-protobuf:${grpcVersion}"
}
`)
	writeCodeUnitFixture(t, root, "src/ads/src/main/java/example/Ads.java", `package example;
import io.grpc.Server;
class Ads {}
`)
	writeCodeUnitFixture(t, root, "src/cart/cartservice.sln", "Microsoft Visual Studio Solution File, Format Version 12.00\n")
	writeCodeUnitFixture(t, root, "src/cart/src/cartservice.csproj", `<Project Sdk="Microsoft.NET.Sdk.Web">
  <PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup>
  <ItemGroup><PackageReference Include="Grpc.AspNetCore" Version="2.80.0" /></ItemGroup>
</Project>`)
	writeCodeUnitFixture(t, root, "src/cart/src/Program.cs", "using Grpc.Core;\nclass Program {}\n")

	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	tests := []struct {
		name         string
		target       *gatewayv1.CodeUnitTarget
		language     string
		module       string
		version      string
		dependencies []string
		sourcePath   string
		imports      []string
	}{
		{
			name: "jvm", target: &gatewayv1.CodeUnitTarget{Id: "shop/ads", Path: "src/ads"},
			language: "jvm", module: "example.ads", version: "21",
			dependencies: []string{"io.grpc:grpc-protobuf@1.82.1"},
			sourcePath:   "src/ads/src/main/java/example/Ads.java", imports: []string{"io.grpc.Server"},
		},
		{
			name: "dotnet", target: &gatewayv1.CodeUnitTarget{Id: "shop/cart", Path: "src/cart"},
			language: "dotnet", module: "cartservice", version: "net10.0",
			dependencies: []string{"Grpc.AspNetCore@2.80.0"},
			sourcePath:   "src/cart/src/Program.cs", imports: []string{"Grpc.Core"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := server.GetProjectInfo(t.Context(), &gatewayv1.GetProjectInfoRequest{CodeUnit: test.target})
			if err != nil {
				t.Fatal(err)
			}
			if response.GetFailure() != nil {
				t.Fatalf("project-info failure = %+v (language=%q hashes=%v)", response.GetFailure(), response.GetLanguage(), response.GetFileHashes())
			}
			if !reflect.DeepEqual(response.GetCodeUnit(), test.target) {
				t.Fatalf("code unit = %+v, want %+v", response.GetCodeUnit(), test.target)
			}
			if response.GetLanguage() != test.language || response.GetModule() != test.module || response.GetLanguageVersion() != test.version {
				t.Fatalf("identity = language %q module %q version %q", response.GetLanguage(), response.GetModule(), response.GetLanguageVersion())
			}
			var dependencies []string
			for _, dependency := range response.GetDependencies() {
				dependencies = append(dependencies, dependency.GetName()+"@"+dependency.GetVersion())
			}
			if !reflect.DeepEqual(dependencies, test.dependencies) {
				t.Fatalf("dependencies = %#v, want %#v", dependencies, test.dependencies)
			}
			if len(response.GetSourceFiles()) != 1 || response.GetSourceFiles()[0].GetPath() != test.sourcePath || !reflect.DeepEqual(response.GetSourceFiles()[0].GetImports(), test.imports) {
				t.Fatalf("source files = %+v", response.GetSourceFiles())
			}
			if _, ok := response.GetFileHashes()[test.sourcePath]; !ok {
				t.Fatalf("repository-relative source hash %q missing from %+v", test.sourcePath, response.GetFileHashes())
			}
		})
	}
}
