package toolbox

import (
	"context"
	"errors"
	"testing"
)

func TestRegistryListsAndCallsTools(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Tool{Name: "echo"}, func(_ context.Context, arguments map[string]string) ([]Content, error) {
		return []Content{TextContent(arguments["text"])}, nil
	}); err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "echo" {
		t.Fatalf("Definitions() = %+v", definitions)
	}
	content, err := registry.Call(context.Background(), "echo", map[string]string{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 1 || content[0].Text != "hello" {
		t.Fatalf("Call() = %+v", content)
	}
}

func TestRegistryReturnsDetachedDefinitions(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Tool{
		Name: "echo",
		InputSchema: InputSchema{
			Required: []string{"text"},
			Properties: map[string]PropertySchema{
				"text": {Type: "string", Enum: []string{"one"}},
			},
		},
	}, func(context.Context, map[string]string) ([]Content, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	definitions[0].InputSchema.Required[0] = "changed"
	property := definitions[0].InputSchema.Properties["text"]
	property.Enum[0] = "changed"
	definitions[0].InputSchema.Properties["text"] = property

	fresh := registry.Definitions()[0]
	if fresh.InputSchema.Required[0] != "text" || fresh.InputSchema.Properties["text"].Enum[0] != "one" {
		t.Fatalf("registry metadata was mutated through returned definition: %+v", fresh)
	}
}

func TestRegistryCloseRejectsCalls(t *testing.T) {
	registry := NewRegistry()
	registry.Close()
	if _, err := registry.Call(context.Background(), "missing", nil); err == nil {
		t.Fatal("Call() after Close() succeeded")
	}
	if err := registry.Register(Tool{Name: "late"}, func(context.Context, map[string]string) ([]Content, error) {
		return nil, nil
	}); err == nil {
		t.Fatal("Register() after Close() succeeded")
	}
	if _, err := NewRegistry().Call(context.Background(), "missing", nil); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("unknown error = %v, want ErrUnknownTool", err)
	}
}
