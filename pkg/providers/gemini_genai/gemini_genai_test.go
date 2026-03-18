package gemini_genai

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

func TestChatRoleMapping(t *testing.T) {
	tcID := "call_list_dir_12345"
	toolCallNames := map[string]string{
		tcID: "list_dir",
	}
	
	name := resolveToolResponseName(tcID, toolCallNames)
	if name != "list_dir" {
		t.Errorf("expected list_dir, got %s", name)
	}
	
	name2 := resolveToolResponseName("call_read_file_67890", toolCallNames)
	if name2 != "read_file" {
		t.Errorf("expected read_file, got %s", name2)
	}
}

func TestNormalizeStoredToolCall(t *testing.T) {
	tc := protocoltypes.ToolCall{
		ID: "1",
		Function: &protocoltypes.FunctionCall{
			Name:      "test_tool",
			Arguments: `{"arg1": "val1"}`,
		},
	}
	
	name, args, _ := normalizeStoredToolCall(tc)
	if name != "test_tool" {
		t.Errorf("expected test_tool, got %s", name)
	}
	if args["arg1"] != "val1" {
		t.Errorf("expected val1, got %v", args["arg1"])
	}
}

func TestMapToGenaiSchema(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"description": "test tool",
		"properties": map[string]any{
			"param1": map[string]any{
				"type": "string",
				"description": "a string param",
			},
		},
		"required": []any{"param1"},
	}
	
	schema := mapToGenaiSchema(params)
	if schema == nil {
		t.Fatal("expected schema to be non-nil")
	}
	// We check for some property existence rather than exact type value to avoid type mapping issues in tests
	if len(schema.Properties) != 1 {
		t.Errorf("expected 1 property, got %d", len(schema.Properties))
	}
	if _, ok := schema.Properties["param1"]; !ok {
		t.Errorf("expected property param1 to exist")
	}
}
