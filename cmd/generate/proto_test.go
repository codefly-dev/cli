package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

func TestProtoGenerationPathArgs(t *testing.T) {
	previous := protoPaths
	t.Cleanup(func() { protoPaths = previous })
	protoPaths = []string{"mind/gateway/v1/gateway.proto", "mind/v1/mind.proto"}

	if got, want := protoGenerationPathArgs("/workspace/proto", false), []string{
		"--path", "mind/gateway/v1/gateway.proto", "--path", "mind/v1/mind.proto",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relative path args = %v, want %v", got, want)
	}
	if got, want := protoGenerationPathArgs("/workspace/proto", true), []string{
		"--path", "/workspace/proto/mind/gateway/v1/gateway.proto",
		"--path", "/workspace/proto/mind/v1/mind.proto",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("absolute path args = %v, want %v", got, want)
	}
}

func TestResolveProtoTemplate(t *testing.T) {
	root := t.TempDir()
	input, output := filepath.Join(root, "proto"), filepath.Join(root, "generated")
	for _, path := range []string{
		filepath.Join(input, "buf.gen.yaml"),
		filepath.Join(output, "buf.gen.local.yaml"),
		filepath.Join(output, "sdk", "buf.gen.yaml"),
	} {
		writeTestFile(t, path, "version: v2\nplugins: []\n")
	}
	for _, tc := range []struct {
		name, template, want string
		local                bool
	}{
		{"default", "", filepath.Join(input, "buf.gen.yaml"), false},
		{"local", "", filepath.Join(output, "buf.gen.local.yaml"), true},
		{"relative", "sdk/buf.gen.yaml", filepath.Join(output, "sdk", "buf.gen.yaml"), false},
		{"explicit overrides local", "sdk/buf.gen.yaml", filepath.Join(output, "sdk", "buf.gen.yaml"), true},
		{"absolute", filepath.Join(input, "buf.gen.yaml"), filepath.Join(input, "buf.gen.yaml"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProtoTemplate(input, output, tc.template, tc.local)
			if err != nil || got != tc.want {
				t.Fatalf("template = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"missing.yaml", "sdk"} {
		if _, err := resolveProtoTemplate(input, output, bad, false); err == nil {
			t.Fatalf("accepted invalid template %q", bad)
		}
	}
}

func TestProtoMountRootNeverExposesFilesystemRoot(t *testing.T) {
	if _, _, err := protoMountRoot("/repo/proto", "/repo/output", "/elsewhere/templates", nil, ""); err == nil {
		t.Fatal("accepted writable mount of the filesystem root")
	}
	root, _, err := protoMountRoot("/repo/proto", "/repo/service/output", "/repo/templates", nil, "")
	if err != nil || root != "/repo" {
		t.Fatalf("mount root = %q, %v", root, err)
	}
}

// The go-grpc service layout keeps buf.gen.yaml in proto/ with its outputs in
// sibling directories. The mount must reach every one of them, or buf writes
// them inside the container and they are lost (#836).
func TestProtoMountRootCoversSiblingOutputs(t *testing.T) {
	workspace := t.TempDir()
	service := filepath.Join(workspace, "mod", "svc")
	input := filepath.Join(service, "proto")
	template := filepath.Join(input, "buf.gen.yaml")
	writeTestFile(t, template, `version: v2
plugins:
  - local: protoc-gen-go
    out: ../code/pkg/gen
    opt: paths=source_relative
  - local: protoc-gen-go-grpc
    out: ../code/pkg/gen
  - local: protoc-gen-openapiv2
    out: ../openapi
`)
	outs, err := protoTemplateOutputs(template)
	if err != nil {
		t.Fatalf("template outputs: %v", err)
	}
	if want := []string{filepath.Join(service, "code", "pkg", "gen"), filepath.Join(service, "openapi")}; !reflect.DeepEqual(outs, want) {
		t.Fatalf("outputs = %v, want %v", outs, want)
	}
	root, _, err := protoMountRoot(input, input, input, outs, workspace)
	if err != nil {
		t.Fatalf("mount root: %v", err)
	}
	if root != service {
		t.Fatalf("mount root = %q, want %q", root, service)
	}
	for _, path := range append([]string{input}, outs...) {
		if !pathWithin(root, path) {
			t.Fatalf("mount root %s does not cover %s", root, path)
		}
	}
}

func TestProtoMountRootRefusesOutputEscapingBoundary(t *testing.T) {
	workspace := t.TempDir()
	input := filepath.Join(workspace, "svc", "proto")
	template := filepath.Join(input, "buf.gen.yaml")
	writeTestFile(t, template, "version: v2\nplugins:\n  - local: protoc-gen-go\n    out: ../../../elsewhere\n")
	outs, err := protoTemplateOutputs(template)
	if err != nil {
		t.Fatalf("template outputs: %v", err)
	}
	if _, _, err := protoMountRoot(input, input, input, outs, workspace); err == nil {
		t.Fatal("accepted an output outside the workspace")
	}
	// Outside a workspace the boundary is the directories the caller named.
	if _, _, err := protoMountRoot(input, input, input, outs, ""); err == nil {
		t.Fatal("accepted an output outside the named directories")
	}
	writeTestFile(t, template, "version: v2\nplugins:\n  - local: protoc-gen-go\n    out: /abs/gen\n")
	if _, err := protoTemplateOutputs(template); err == nil {
		t.Fatal("accepted an absolute output the companion cannot see")
	}
}

func TestProtoMountBoundaryIsTheOwningWorkspace(t *testing.T) {
	workspace := t.TempDir()
	writeTestFile(t, filepath.Join(workspace, "workspace.codefly.yaml"), "name: example\n")
	input := filepath.Join(workspace, "mod", "svc", "proto")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := protoMountBoundary(context.Background(), input)
	if err != nil || got != workspace {
		t.Fatalf("boundary = %q, %v; want %q", got, err, workspace)
	}
}

// What the staged design has to get right, and what the mtime guard got
// wrong: buf syncs an output tree rather than rewriting it, so neither a
// write nor a file already on disk is evidence that *this* generation
// produced anything (#885, and the stale-file hole that followed it). The
// evidence is the staging tree, and these pin that.

// stageFile writes one file the companion is pretending to have generated.
func stageFile(t *testing.T, staging string, out int, rel, contents string) {
	t.Helper()
	writeTestFile(t, filepath.Join(protoStagingSlot(staging, out), filepath.FromSlash(rel)), contents)
}

func TestProtoStagingTemplateRedirectsEveryOutputAndChangesNothingElse(t *testing.T) {
	service := t.TempDir()
	template := filepath.Join(service, "proto", "buf.gen.yaml")
	writeTestFile(t, template, `version: v1
managed:
  enabled: false
plugins:
  - name: go
    path: protoc-gen-go
    out: ../code/pkg/gen
    opt:
      - paths=source_relative
  - name: go-grpc
    path: protoc-gen-go-grpc
    out: ../code/pkg/gen
  - name: openapiv2
    path: protoc-gen-openapiv2
    out: ../openapi
`)
	outs, err := protoTemplateOutputs(template)
	if err != nil {
		t.Fatalf("template outputs: %v", err)
	}
	staged, err := protoStagingTemplate(template, outs, "/workspace/.proto-gen-1-staging")
	if err != nil {
		t.Fatalf("staged template: %v", err)
	}

	var document struct {
		Version string `yaml:"version"`
		Managed struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"managed"`
		Plugins []struct {
			Name string   `yaml:"name"`
			Path string   `yaml:"path"`
			Out  string   `yaml:"out"`
			Opt  []string `yaml:"opt"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(staged, &document); err != nil {
		t.Fatalf("parse staged template: %v\n%s", err, staged)
	}
	if document.Version != "v1" || document.Managed.Enabled {
		t.Fatalf("staging rewrote more than the outputs: %s", staged)
	}
	if len(document.Plugins) != 3 {
		t.Fatalf("staged plugins = %d, want 3: %s", len(document.Plugins), staged)
	}
	// The go-grpc layout's four Go plugins share one `out`; sharing must
	// survive, or one output's files land in two places.
	gen, openapi := document.Plugins[0].Out, document.Plugins[2].Out
	if gen != document.Plugins[1].Out {
		t.Fatalf("plugins sharing an output were split: %q vs %q", gen, document.Plugins[1].Out)
	}
	if gen == openapi {
		t.Fatalf("distinct outputs were merged into %q", gen)
	}
	for _, slot := range []string{gen, openapi} {
		if !strings.HasPrefix(slot, "/workspace/.proto-gen-1-staging/") {
			t.Fatalf("output %q was not redirected into staging", slot)
		}
	}
	// Everything else the caller declared is the caller's.
	if document.Plugins[0].Path != "protoc-gen-go" || len(document.Plugins[0].Opt) != 1 || document.Plugins[0].Opt[0] != "paths=source_relative" {
		t.Fatalf("staging changed a plugin's own declaration: %s", staged)
	}
}

func TestProtoStagingTemplateRefusesATemplateWithNothingToGenerate(t *testing.T) {
	root := t.TempDir()
	template := filepath.Join(root, "buf.gen.yaml")
	writeTestFile(t, template, "version: v2\nplugins: []\n")
	if _, err := protoStagingTemplate(template, nil, "/workspace/.staging"); err == nil {
		t.Fatal("staged a template declaring no plugins")
	}
}

func TestProtoTemplateClean(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, contents string
		want           bool
	}{
		{"absent", "version: v2\nplugins: []\n", false},
		{"false", "version: v2\nclean: false\nplugins: []\n", false},
		{"true", "version: v2\nclean: true\nplugins: []\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := filepath.Join(root, tc.name+".yaml")
			writeTestFile(t, template, tc.contents)
			got, err := protoTemplateClean(template)
			if err != nil || got != tc.want {
				t.Fatalf("clean = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// The hole the stale-file review found: a plugin that emits nothing leaves the
// output holding whatever was already there, and that file is not evidence of
// generation. Decided from staging alone, and before anything is published, so
// the refused run leaves the output exactly as it found it.
func TestPublishProtoOutputsRefusesAGenerationThatEmittedNothing(t *testing.T) {
	root, outs := protoOutputFixture(t)
	staging := t.TempDir()
	stale := filepath.Join(outs[0], "stale.txt")
	writeTestFile(t, stale, "left over from some earlier run\n")
	before, err := os.Stat(stale)
	if err != nil {
		t.Fatal(err)
	}

	err = publishProtoOutputs(staging, root, outs, false, filepath.Join(root, "buf.gen.yaml"))
	if err == nil {
		t.Fatal("a pre-existing file was accepted as proof of generation")
	}
	if !strings.Contains(err.Error(), "produced no file") {
		t.Fatalf("error does not name the cause: %v", err)
	}
	after, err := os.Stat(stale)
	if err != nil {
		t.Fatalf("the refused generation disturbed the output: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("the refused generation rewrote the output")
	}
}

// Even with `clean: true`, which would otherwise replace the output: a
// generation that emitted nothing must not be allowed to empty a consumer's
// tree.
func TestPublishProtoOutputsKeepsTheOutputWhenNothingWasEmittedUnderClean(t *testing.T) {
	root, outs := protoOutputFixture(t)
	writeTestFile(t, filepath.Join(outs[0], "api_pb.ts"), "export {};\n")
	if err := publishProtoOutputs(t.TempDir(), root, outs, true, filepath.Join(root, "buf.gen.yaml")); err == nil {
		t.Fatal("accepted a generation that emitted nothing")
	}
	if _, err := os.Stat(filepath.Join(outs[0], "api_pb.ts")); err != nil {
		t.Fatalf("clean emptied the output for a generation that produced nothing: %v", err)
	}
}

func TestPublishProtoOutputsPublishesWhatWasEmitted(t *testing.T) {
	root, outs := protoOutputFixture(t)
	staging := t.TempDir()
	stageFile(t, staging, 0, "documents/v1/api_pb.ts", "export const api = 1;\n")
	stageFile(t, staging, 1, "api.swagger.json", "{}\n")

	if err := publishProtoOutputs(staging, root, outs, false, filepath.Join(root, "buf.gen.yaml")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(outs[0], "documents", "v1", "api_pb.ts"): "export const api = 1;\n",
		filepath.Join(outs[1], "api.swagger.json"):             "{}\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

// The #885 case, end to end through publication: the output already holds
// exactly what this run emitted. It must succeed and leave the tree alone,
// content and modification time, because that is what a drift gate reads.
func TestPublishProtoOutputsLeavesAnUnchangedReplayUntouched(t *testing.T) {
	root, outs := protoOutputFixture(t)
	staging := t.TempDir()
	published := filepath.Join(outs[0], "documents", "v1", "api_pb.ts")
	stageFile(t, staging, 0, "documents/v1/api_pb.ts", "export const api = 1;\n")
	writeTestFile(t, published, "export const api = 1;\n")
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(published, stale, stale); err != nil {
		t.Fatal(err)
	}

	if err := publishProtoOutputs(staging, root, outs, false, filepath.Join(root, "buf.gen.yaml")); err != nil {
		t.Fatalf("refused an unchanged replay: %v", err)
	}
	info, err := os.Stat(published)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(stale.Add(time.Second)) {
		t.Fatalf("an unchanged replay rewrote %s (mtime moved from %s to %s)", published, stale, info.ModTime())
	}
}

// A changed input has to reach the output, and an output that is a shared
// source root must keep its handwritten neighbours — core's FormatGoOutputs
// only formats files carrying the generated-code notice precisely because
// `out` may hold both.
func TestPublishProtoOutputsOverwritesGeneratedAndKeepsHandwritten(t *testing.T) {
	root, outs := protoOutputFixture(t)
	staging := t.TempDir()
	stageFile(t, staging, 0, "api_pb.ts", "export const api = 2;\n")
	writeTestFile(t, filepath.Join(outs[0], "api_pb.ts"), "export const api = 1;\n")
	writeTestFile(t, filepath.Join(outs[0], "handwritten.ts"), "export const mine = true;\n")

	if err := publishProtoOutputs(staging, root, outs, false, filepath.Join(root, "buf.gen.yaml")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outs[0], "api_pb.ts"))
	if err != nil || string(got) != "export const api = 2;\n" {
		t.Fatalf("changed generation did not reach the output: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outs[0], "handwritten.ts")); err != nil {
		t.Fatalf("publication removed a handwritten neighbour: %v", err)
	}
}

// `clean: true` is the caller asking for the output to be what this run
// emitted, which is the pruning buf would have done itself.
func TestPublishProtoOutputsCleanReplacesTheOutput(t *testing.T) {
	root, outs := protoOutputFixture(t)
	staging := t.TempDir()
	stageFile(t, staging, 0, "api_pb.ts", "export const api = 1;\n")
	writeTestFile(t, filepath.Join(outs[0], "gone_pb.ts"), "export {};\n")

	if err := publishProtoOutputs(staging, root, outs, true, filepath.Join(root, "buf.gen.yaml")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outs[0], "gone_pb.ts")); !os.IsNotExist(err) {
		t.Fatalf("clean kept a file this run did not generate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outs[0], "api_pb.ts")); err != nil {
		t.Fatalf("clean did not publish what was emitted: %v", err)
	}
}

// One plugin emitting nothing is not a failed generation — openapiv2 emits
// nothing for a contract carrying no REST annotations — and must not empty
// that output either.
func TestPublishProtoOutputsToleratesOneOutputWithNothingToEmit(t *testing.T) {
	root, outs := protoOutputFixture(t)
	staging := t.TempDir()
	stageFile(t, staging, 0, "api_pb.ts", "export {};\n")
	writeTestFile(t, filepath.Join(outs[1], "api.swagger.json"), "{}\n")

	if err := publishProtoOutputs(staging, root, outs, false, filepath.Join(root, "buf.gen.yaml")); err != nil {
		t.Fatalf("refused a template whose second plugin had nothing to emit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outs[1], "api.swagger.json")); err != nil {
		t.Fatalf("an output with nothing emitted was disturbed: %v", err)
	}
}

// Staging must survive `clean: true` over an output that would have contained
// it. An `out` can resolve to the generation mount's own root (`out: .` in a
// template that is its own output directory), so a staging tree inside that
// mount is deleted along with the output — and publication then reads files
// that no longer exist. Staging therefore lives where no `out` can name it,
// and this pins that a clean of the whole tree still publishes.
func TestPublishProtoOutputsSurvivesACleanOfTheTreeThatWouldHaveHeldStaging(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	root := t.TempDir()
	out := root // `out: .` — the output IS the tree staging used to live under
	staging, err := newProtoStaging()
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(staging) })
	// The invariant that makes the clean below survivable, asserted directly:
	// staging is somewhere no `out` can name.
	if pathWithin(out, staging) {
		t.Fatalf("staging %s is inside the output %s; a clean of the output would delete the evidence", staging, out)
	}
	stageFile(t, staging, 0, "api_pb.ts", "export const api = 1;\n")
	writeTestFile(t, filepath.Join(out, "gone_pb.ts"), "export {};\n")

	if err := publishProtoOutputs(staging, out, []string{out}, true, "buf.gen.yaml"); err != nil {
		t.Fatalf("clean of the output holding the mount root broke publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "api_pb.ts")); err != nil {
		t.Fatalf("nothing was published after the clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "gone_pb.ts")); !os.IsNotExist(err) {
		t.Fatalf("clean kept a file this run did not generate: %v", err)
	}
}

// Outputs nest: a plugin writing `nested/x.ts` into the slot for `out: gen`
// publishes it to the path `out: gen/nested` owns. Cleaning each output just
// before copying it therefore deletes what a sibling already published, so
// every destination is cleaned before anything is published.
func TestPublishProtoOutputsCleansEveryNestedOutputBeforePublishingAny(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "gen")
	nested := filepath.Join(parent, "nested")
	staging := t.TempDir()
	stageFile(t, staging, 0, "nested/fromparent_pb.ts", "export const a = 1;\n")
	stageFile(t, staging, 1, "fromnested_pb.ts", "export const b = 2;\n")
	writeTestFile(t, filepath.Join(parent, "gone_pb.ts"), "export {};\n")
	writeTestFile(t, filepath.Join(nested, "gone_too_pb.ts"), "export {};\n")

	if err := publishProtoOutputs(staging, filepath.Dir(parent), []string{parent, nested}, true, "buf.gen.yaml"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Both outputs' files must be present: the nested clean must not have
	// erased what the parent slot published into it, nor the reverse.
	for path, want := range map[string]string{
		filepath.Join(nested, "fromparent_pb.ts"): "export const a = 1;\n",
		filepath.Join(nested, "fromnested_pb.ts"): "export const b = 2;\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, gone := range []string{filepath.Join(parent, "gone_pb.ts"), filepath.Join(nested, "gone_too_pb.ts")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("clean kept %s: %v", gone, err)
		}
	}
}

// Staging is evidence only if it starts empty and belongs to one run. A
// directory that already exists cannot be either, so creation is exclusive —
// two runs in the same millisecond get different trees, and a leftover from a
// killed run can never supply files this run did not generate.
func TestNewProtoStagingIsExclusiveAndEmpty(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	first, err := newProtoStaging()
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	second, err := newProtoStaging()
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if first == second {
		t.Fatalf("two runs were handed the same staging tree %s", first)
	}
	for _, staging := range []string{first, second} {
		entries, err := os.ReadDir(staging)
		if err != nil {
			t.Fatalf("read %s: %v", staging, err)
		}
		if len(entries) != 0 {
			t.Fatalf("staging tree %s is not empty: %v", staging, entries)
		}
		// Outside any generation mount, so no `out` can name it and
		// `clean: true` can never reach it.
		if files, err := protoStagedFiles(protoStagingSlot(staging, 0)); err != nil || len(files) != 0 {
			t.Fatalf("a fresh staging slot reported %v, %v", files, err)
		}
	}
}

// Publication must not be redirected by the output's own contents. A directory
// in the published tree that is a symlink out of the output would otherwise
// have the host overwrite a file the template never declared, on a path
// outside every `out`.
func TestPublishProtoOutputsCannotFollowASymlinkOutOfTheOutput(t *testing.T) {
	out, staging, outside := t.TempDir(), t.TempDir(), t.TempDir()
	guarded := filepath.Join(outside, "value.ts")
	writeTestFile(t, guarded, "preserve")
	if err := os.Symlink(outside, filepath.Join(out, "nested")); err != nil {
		t.Fatal(err)
	}
	stageFile(t, staging, 0, "nested/value.ts", "overwritten")

	err := publishProtoOutputs(staging, out, []string{out}, false, "buf.gen.yaml")
	if err == nil {
		t.Fatal("published through a symlink leaving the declared output")
	}
	if !strings.Contains(err.Error(), out) {
		t.Fatalf("error does not name the output: %v", err)
	}
	got, readErr := os.ReadFile(guarded)
	if readErr != nil || string(got) != "preserve" {
		t.Fatalf("a file outside the declared output was overwritten: %q, %v", got, readErr)
	}
}

// A relative symlink that stays inside the output is the output's own
// business, and publication writes through it as buf did.
func TestPublishProtoOutputsFollowsASymlinkThatStaysInsideTheOutput(t *testing.T) {
	out, staging := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(out, "nested")); err != nil {
		t.Fatal(err)
	}
	stageFile(t, staging, 0, "nested/value.ts", "published")

	if err := publishProtoOutputs(staging, out, []string{out}, false, "buf.gen.yaml"); err != nil {
		t.Fatalf("refused a symlink that stays inside the output: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "real", "value.ts"))
	if err != nil || string(got) != "published" {
		t.Fatalf("value.ts = %q, %v; want \"published\"", got, err)
	}
}

// Publication must never delete its own evidence. generateProtoCode cannot
// produce a staging tree inside a declared output — staging is created outside
// the generation mount — but the trap is worth closing in the function too,
// since a `clean: true` over that output would otherwise remove the staged
// files and then fail to read them.
func TestPublishProtoOutputsCleanKeepsAStagingTreeInsideTheOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gen")
	staging := filepath.Join(out, ".stage")
	stageFile(t, staging, 0, "api_pb.ts", "export const api = 1;\n")
	writeTestFile(t, filepath.Join(out, "gone_pb.ts"), "export {};\n")

	if err := publishProtoOutputs(staging, filepath.Dir(out), []string{out}, true, "buf.gen.yaml"); err != nil {
		t.Fatalf("clean deleted the staged source it was about to publish: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "api_pb.ts"))
	if err != nil || string(got) != "export const api = 1;\n" {
		t.Fatalf("api_pb.ts = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(out, "gone_pb.ts")); !os.IsNotExist(err) {
		t.Fatalf("clean kept a file this run did not generate: %v", err)
	}
}

// An output that IS the staging tree has no sane clean, and silently emptying
// it would destroy the generation.
func TestPublishProtoOutputsRefusesAnOutputThatIsTheStagingTree(t *testing.T) {
	staging := t.TempDir()
	stageFile(t, staging, 0, "api_pb.ts", "export {};\n")
	if err := publishProtoOutputs(staging, staging, []string{staging}, true, "buf.gen.yaml"); err == nil {
		t.Fatal("accepted an output that is the staging tree itself")
	}
	if _, err := os.Stat(filepath.Join(protoStagingSlot(staging, 0), "api_pb.ts")); err != nil {
		t.Fatalf("the refusal still destroyed the staged generation: %v", err)
	}
}

// A lexical boundary check is bypassed by a symlinked component in the output's
// own path: `out: <workspace>/link/gen` with `link` pointing outside reads as
// inside the workspace, and then `clean: true` deletes the resolved target
// before publication ever runs. The escape test therefore resolves both sides.
func TestProtoMountRootRefusesAnOutputReachedThroughAnEscapingSymlink(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "gen"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(workspace, "svc", "proto")
	escaping := filepath.Join(workspace, "link", "gen")

	_, _, err := protoMountRoot(input, input, input, []string{escaping}, workspace)
	if err == nil {
		t.Fatal("accepted an output that leaves the workspace through a symlink")
	}
	if !strings.Contains(err.Error(), "through a symlink") || !strings.Contains(err.Error(), outside) {
		t.Fatalf("error does not explain the escape: %v", err)
	}

	// The same shape staying inside the workspace is fine, and an output buf
	// has yet to create still resolves as far as it exists.
	inside := filepath.Join(workspace, "real")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(workspace, "ok")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := protoMountRoot(input, input, input, []string{filepath.Join(workspace, "ok", "gen")}, workspace); err != nil {
		t.Fatalf("refused an output reached through a symlink that stays inside: %v", err)
	}
}

// Even handed an escaping output directly, publication must not act outside
// the boundary: `clean: true` deletes before it writes, and a check alone
// cannot survive a symlink swapped in after it.
func TestPublishProtoOutputsCleanCannotDeleteThroughAnEscapingSymlink(t *testing.T) {
	workspace, outside, staging := t.TempDir(), t.TempDir(), t.TempDir()
	guarded := filepath.Join(outside, "gen", "value.ts")
	writeTestFile(t, guarded, "preserve")
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(workspace, "link", "gen")
	stageFile(t, staging, 0, "api_pb.ts", "export {};\n")

	if err := publishProtoOutputs(staging, workspace, []string{out}, true, "buf.gen.yaml"); err == nil {
		t.Fatal("cleaned an output that resolves outside the boundary")
	}
	got, err := os.ReadFile(guarded)
	if err != nil || string(got) != "preserve" {
		t.Fatalf("clean deleted a tree outside the boundary: %q, %v", got, err)
	}
}

// Teardown is housekeeping, not generation. A removal that fails leaves the
// state an interrupted generate leaves, which the recovery sweep collects — so
// with ownership projected it must not turn a correct generation into a failed
// one, and the leftover must stay visible. Without ownership nothing collects
// it, and then the leak is the command's to report.
func TestProtoTeardownOutcome(t *testing.T) {
	teardown := errors.New("Docker.Shutdown: cannot remove container: context deadline exceeded")

	if err := protoTeardownOutcome(teardown, "proto-gen-1", true); err != nil {
		t.Fatalf("a recoverable leftover failed the command: %v", err)
	}

	err := protoTeardownOutcome(teardown, "proto-gen-2", false)
	if err == nil {
		t.Fatal("an unrecoverable leftover was not reported")
	}
	// The report has to be actionable: it names the container nothing will
	// collect, and keeps the cause.
	if !strings.Contains(err.Error(), "proto-gen-2") {
		t.Fatalf("error does not name the leftover container: %v", err)
	}
	if !errors.Is(err, teardown) {
		t.Fatalf("error does not preserve the teardown cause: %v", err)
	}
}

// The companion piece: a generation that actually failed must still fail,
// whatever teardown did. generateProtoCode joins the teardown outcome onto the
// result rather than replacing it, so a nil teardown outcome cannot mask a
// generation error — this pins the join, which is the part a refactor could
// quietly invert.
func TestProtoTeardownOutcomeNeverMasksAGenerationError(t *testing.T) {
	generation := errors.New("cannot generate proto code")
	teardown := errors.New("cannot remove container")

	// What the defer computes, for a recoverable leftover: nothing to add.
	joined := errors.Join(generation, protoTeardownOutcome(teardown, "proto-gen-3", true))
	if !errors.Is(joined, generation) {
		t.Fatalf("a successful-teardown join dropped the generation error: %v", joined)
	}
	if errors.Is(joined, teardown) {
		t.Fatalf("a recoverable teardown failure was reported as a command failure: %v", joined)
	}

	// And for an unrecoverable one: both survive, so neither cause is lost.
	joined = errors.Join(generation, protoTeardownOutcome(teardown, "proto-gen-4", false))
	if !errors.Is(joined, generation) || !errors.Is(joined, teardown) {
		t.Fatalf("join lost a cause: %v", joined)
	}
}

func protoOutputFixture(t *testing.T) (root string, outs []string) {
	t.Helper()
	root = t.TempDir()
	outs = []string{filepath.Join(root, "sdk", "src", "gen"), filepath.Join(root, "svc", "openapi")}
	return root, outs
}

// faithfulStagingMount stands in for a companion whose staging directory
// really is the host's, by translating the container path back and removing
// the probe.
func faithfulStagingMount(hostStaging, containerStaging string) protoCommand {
	return func(_ context.Context, bin string, args ...string) error {
		if bin != "rm" {
			return fmt.Errorf("unexpected command %q", bin)
		}
		for _, arg := range args {
			if arg == "--" {
				continue
			}
			rel, err := filepath.Rel(containerStaging, arg)
			if err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(hostStaging, rel)); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestVerifyProtoStagingMountAcceptsAMountTheHostCanRead(t *testing.T) {
	staging := t.TempDir()
	if err := verifyProtoStagingMount(context.Background(), faithfulStagingMount(staging, "/workspace/.staging"), staging, "/workspace/.staging", ".probe"); err != nil {
		t.Fatalf("refused a live staging mount: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(staging, ".probe")); !os.IsNotExist(err) {
		t.Fatalf("probe left behind: %v", err)
	}
}

func TestVerifyProtoStagingMountRefusesAStagingTreeTheHostCannotSee(t *testing.T) {
	staging := t.TempDir()
	blind := func(_ context.Context, _ string, _ ...string) error {
		return fmt.Errorf("rm: can't remove '/workspace/.staging/.probe': No such file or directory")
	}
	err := verifyProtoStagingMount(context.Background(), blind, staging, "/workspace/.staging", ".probe")
	if err == nil {
		t.Fatal("accepted a staging directory the companion cannot reach")
	}
	if !strings.Contains(err.Error(), staging) {
		t.Fatalf("error does not name the staging directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(staging, ".probe")); !os.IsNotExist(err) {
		t.Fatalf("probe left behind after a failed check: %v", err)
	}
}

// A mount the companion reads but whose writes never reach the host: the
// command exits clean and the probe survives. Exit status alone would pass
// this, which is why the host checks the probe is gone.
func TestVerifyProtoStagingMountRefusesAMountThatDiscardsWrites(t *testing.T) {
	staging := t.TempDir()
	discarding := func(_ context.Context, _ string, _ ...string) error { return nil }
	if err := verifyProtoStagingMount(context.Background(), discarding, staging, "/workspace/.staging", ".probe"); err == nil {
		t.Fatal("accepted a mount that discards what the companion writes")
	}
	if _, err := os.Lstat(filepath.Join(staging, ".probe")); !os.IsNotExist(err) {
		t.Fatalf("probe left behind: %v", err)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
