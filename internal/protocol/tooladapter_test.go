package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestToolArgumentAdapterRoundTrip(t *testing.T) {
	renames := NewToolRenames()
	if !renames.BindAdapter("read", "read", map[string]string{"path": "file_path"}) || renames.Empty() {
		t.Fatal("same-name argument adapter was not registered")
	}
	original := `{"path":"a.txt","limit":9007199254740993,"metadata":{"path":"nested"},"label":"path oldString"}`
	converted, changed := renames.ToClientArguments("read", original)
	if !changed {
		t.Fatal("read arguments were not adapted")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(converted.(string)), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["file_path"]) != `"a.txt"` || got["path"] != nil || string(got["limit"]) != "9007199254740993" || string(got["metadata"]) != `{"path":"nested"}` {
		t.Fatalf("argument values changed: %s", converted)
	}
	restored, changed := renames.ToWireArguments("read", converted)
	if !changed {
		t.Fatal("history was not adapted back")
	}
	var want, actual any
	json.Unmarshal([]byte(original), &want)
	json.Unmarshal([]byte(restored.(string)), &actual)
	if !reflect.DeepEqual(want, actual) {
		t.Fatalf("round trip differs: %s", restored)
	}
}

func TestToolArgumentAdapterPreservesInvalidAndUnrelatedArguments(t *testing.T) {
	renames := NewToolRenames()
	renames.BindAdapter("Edit", "edit", map[string]string{"path": "file_path", "oldString": "old_string", "newString": "new_string"})
	for _, arguments := range []string{`{"path":`, `null`, `[]`, `{"path":"a","file_path":"b"}`, `{"file_path":"a"}`, `{"path":"a"} trailing`} {
		if converted, changed := renames.ToClientArguments("edit", arguments); changed || converted != arguments {
			t.Fatalf("invalid/conflicting/unchanged arguments modified: %q -> %v", arguments, converted)
		}
	}
	original := map[string]any{"path": "a", "oldString": "old", "newString": "new", "sandbox_permissions": "workspace-write"}
	converted, changed := renames.ToClientArguments("edit", original)
	if !changed || converted.(map[string]any)["old_string"] != "old" || converted.(map[string]any)["sandbox_permissions"] != "workspace-write" {
		t.Fatalf("edit map not converted: %#v", converted)
	}
	if original["path"] != "a" || original["file_path"] != nil {
		t.Fatal("conversion mutated the source")
	}
	if _, changed := renames.ToClientArguments("unknown", original); changed {
		t.Fatal("unregistered tool adapted")
	}
	if renames.BindAdapter("Other", "edit", map[string]string{"path": "other"}) {
		t.Fatal("ambiguous wire name was accepted")
	}
	if renames.BindAdapter("Broken", "broken", map[string]string{"a": "x", "b": "x"}) {
		t.Fatal("non-invertible field mapping was accepted")
	}
}

func TestToolAdapterOfficialHistoryFields(t *testing.T) {
	r := NewToolRenames()
	r.BindAdapter("Edit", "edit", map[string]string{"path": "file_path", "oldString": "old_string", "newString": "new_string", "replaceAll": "replace_all"}, "path", "oldString", "newString", "replaceAll")
	client := map[string]any{"file_path": "fixture.txt", "old_string": "before", "new_string": "after", "replace_all": true, "sandbox_permissions": "workspace-write", "justification": "test"}
	wire := map[string]any{"path": "fixture.txt", "oldString": "before", "newString": "after", "replaceAll": true}
	for _, encoded := range []bool{false, true} {
		var input any = client
		if encoded {
			data, _ := json.Marshal(client)
			input = string(data)
		}
		converted, changed := r.ToWireArguments("Edit", input)
		if !changed {
			t.Fatal("history was not adapted")
		}
		var got map[string]any
		if text, ok := converted.(string); ok {
			json.Unmarshal([]byte(text), &got)
		} else {
			got = converted.(map[string]any)
		}
		if !reflect.DeepEqual(got, wire) {
			t.Fatalf("client-only fields leaked: %#v", got)
		}
	}
	canonical := map[string]any{"path": "fixture.txt", "oldString": "before", "newString": "after", "sandbox_permissions": "workspace-write"}
	converted, changed := r.ToWireArguments("edit", canonical)
	if !changed || converted.(map[string]any)["sandbox_permissions"] != nil {
		t.Fatalf("canonical history leaked local permission: %#v", converted)
	}
	if client["sandbox_permissions"] != "workspace-write" || canonical["sandbox_permissions"] != "workspace-write" {
		t.Fatal("history conversion mutated local permission data")
	}
	converted, changed = r.ToClientArguments("edit", wire)
	wantClient := map[string]any{"file_path": "fixture.txt", "old_string": "before", "new_string": "after", "replace_all": true}
	if !changed || !reflect.DeepEqual(converted, wantClient) {
		t.Fatalf("official arguments were not restored for DSH: %#v", converted)
	}
}

func TestRenameResponseToolAdaptsArguments(t *testing.T) {
	cases := []struct {
		name     string
		protocol Protocol
		body     string
	}{
		{"chat", Chat, `{"choices":[{"message":{"tool_calls":[{"id":"call_7","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]}}]}`},
		{"legacy chat", Chat, `{"choices":[{"message":{"function_call":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}}]}`},
		{"responses", Responses, `{"output":[{"type":"function_call","call_id":"call_7","name":"read","arguments":"{\"path\":\"a.txt\"}"}]}`},
		{"response envelope", Responses, `{"response":{"output":[{"type":"function_call","call_id":"call_7","name":"read","arguments":"{\"path\":\"a.txt\"}"}]}}`},
		{"anthropic", Anthropic, `{"content":[{"type":"tool_use","id":"call_7","name":"read","input":{"path":"a.txt"}}]}`},
		{"anthropic large number", Anthropic, `{"content":[{"type":"tool_use","id":"call_7","name":"read","input":{"path":"a.txt","nested":{"value":9007199254740993}}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renames := NewToolRenames()
			renames.BindAdapter("read", "read", map[string]string{"path": "file_path"})
			result := RenameResponseTool([]byte(tc.body), tc.protocol, renames)
			if strings.Contains(tc.body, "9007199254740993") && !strings.Contains(string(result), "9007199254740993") {
				t.Fatalf("large integer changed during argument adaptation: %s", result)
			}
			var payload any
			if err := json.Unmarshal(result, &payload); err != nil {
				t.Fatal(err)
			}
			var found bool
			var visit func(any)
			visit = func(value any) {
				switch value := value.(type) {
				case map[string]any:
					if value["name"] == "read" {
						arguments := value["arguments"]
						if text, ok := arguments.(string); ok {
							json.Unmarshal([]byte(text), &arguments)
						}
						if arguments == nil {
							arguments = value["input"]
						}
						if args, ok := arguments.(map[string]any); ok {
							found = args["file_path"] == "a.txt" && args["path"] == nil
						}
					}
					for _, child := range value {
						visit(child)
					}
				case []any:
					for _, child := range value {
						visit(child)
					}
				}
			}
			visit(payload)
			if !found {
				t.Fatalf("response contract not restored: %s", result)
			}
		})
	}
}
