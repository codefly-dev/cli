package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// hopPolicyProto is authority.hop.v1's policy.proto as a module's contract
// ships it beside the methods it marks.
func hopPolicyProto() *descriptorpb.FileDescriptorProto {
	repeatedString := stringField("actions", 2)
	repeatedString.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	requires := selfField("requires", 1, ".authority.hop.v1.Requirement")
	requires.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	return &descriptorpb.FileDescriptorProto{
		Name:       googleproto.String("authority/hop/v1/policy.proto"),
		Package:    googleproto.String("authority.hop.v1"),
		Syntax:     googleproto.String("proto3"),
		Dependency: []string{"google/protobuf/descriptor.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			descriptorMessage("Policy", requires, boolField("any_checked_caller", 2)),
			descriptorMessage("Requirement", stringField("resource_kind", 1), repeatedString),
		},
		Extension: []*descriptorpb.FieldDescriptorProto{{
			Name: googleproto.String("policy"), Number: googleproto.Int32(51100),
			Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: googleproto.String(".authority.hop.v1.Policy"),
			Extendee: googleproto.String(".google.protobuf.MethodOptions"), JsonName: googleproto.String("policy"),
		}},
	}
}

// hopPolicyMethod carries only `requires: [{resource_kind: kind, actions}]`,
// as the unknown bytes a reader without the Go type holds.
func hopPolicyMethod(name, kind string, actions ...string) *descriptorpb.MethodDescriptorProto {
	var requirement []byte
	requirement = protowire.AppendTag(requirement, 1, protowire.BytesType)
	requirement = protowire.AppendString(requirement, kind)
	for _, action := range actions {
		requirement = protowire.AppendTag(requirement, 2, protowire.BytesType)
		requirement = protowire.AppendString(requirement, action)
	}
	var policy []byte
	policy = protowire.AppendTag(policy, 1, protowire.BytesType)
	policy = protowire.AppendBytes(policy, requirement)
	method := markedMethod(name, ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil)
	method.Options = &descriptorpb.MethodOptions{}
	var raw []byte
	raw = protowire.AppendTag(raw, 51100, protowire.BytesType)
	raw = protowire.AppendBytes(raw, policy)
	method.Options.ProtoReflect().SetUnknown(raw)
	return method
}

// withHopPolicy adds the policy declaration to a published descriptor set.
func withHopPolicy(t *testing.T, contract []byte) []byte {
	t.Helper()
	var set descriptorpb.FileDescriptorSet
	require.NoError(t, googleproto.Unmarshal(contract, &set))
	set.File = append(set.File, hopPolicyProto())
	out, err := googleproto.Marshal(&set)
	require.NoError(t, err)
	return out
}

// A method declaring only its authority policy is an operation, and a tool:
// a read is recomputed with no receipt; a stream is listed, not refused.
func TestGenerateRunnablesDerivesEveryMethodBehindTheHop(t *testing.T) {
	ctx := context.Background()
	streaming := hopPolicyMethod("StreamText", "documents", "read")
	streaming.ServerStreaming = googleproto.Bool(true)
	// exclude carried as the unknown bytes a contract on disk holds (51004 = true).
	excluded := hopPolicyMethod("PurgeText", "documents", "write")
	raw := excluded.Options.ProtoReflect().GetUnknown()
	raw = protowire.AppendTag(raw, 51004, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 1)
	excluded.Options.ProtoReflect().SetUnknown(raw)
	// A method whose derived release name exceeds 50 characters.
	long := hopPolicyMethod("GetDocumentTransformationReceipt", "documents", "read")
	file := ingestionFile(conformingOperation(), hopPolicyMethod("CountText", "documents", "read"),
		hopPolicyMethod("WriteText", "documents", "write"), streaming, excluded, long)
	root, moduleDir := saveRunnableFixture(t, ctx, withHopPolicy(t, descriptorSet(t, file)), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))

	read := func(method string) map[string]any {
		data, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir), "runtime-worker", "grpc", method, runnablespkg.OperationFileName))
		require.NoError(t, err)
		var document map[string]any
		require.NoError(t, json.Unmarshal(data, &document))
		return document
	}
	count := read("CountText")
	require.Equal(t, "RECOVERY_RECOMPUTE", count["recovery"])
	require.Empty(t, count["lookup_scopes"])
	require.Equal(t, "EFFECT_READ_ONLY", count["tool"].(map[string]any)["effect"])
	require.Equal(t, "documents.ingestion", count["audience"])

	write := read("WriteText")
	require.Equal(t, "RECOVERY_RECEIPT", write["recovery"])
	require.Equal(t, "EFFECT_MUTATION", write["tool"].(map[string]any)["effect"])
	require.EqualValues(t, 1, write["max_attempts"])
	// The receipt is read under the scopes that made the effect: no read is
	// added on the written kind, and no lookup scopes are declared.
	require.Equal(t, []any{map[string]any{"resource_kind": "documents", "actions": []any{"write"}}}, write["invoke_scopes"])
	require.Empty(t, write["lookup_scopes"])

	// The explicit option keeps its document, beside its presentation.
	apply := read("ApplyText")
	require.Nil(t, apply["recovery"])
	require.Equal(t, "ApplyText", apply["tool"].(map[string]any)["name"])

	index := readIndex(t, moduleDir)
	require.Len(t, index.Operations, 4, "the stream and the excluded method are calls, not operations")
	for _, entry := range index.Operations {
		require.LessOrEqual(t, len(entry.Name), 50, entry.Name)
		require.NotContains(t, entry.Method, "PurgeText")
		if strings.HasSuffix(entry.Method, "/GetDocumentTransformationReceipt") {
			require.Equal(t, derivedName("runtime-worker", "grpc", "GetDocumentTransformationReceipt"), entry.Name)
			require.Regexp(t, `^runtime-worker-grpc-get-document-transf.*-[0-9a-f]{8}$`, entry.Name)
		}
	}
}
