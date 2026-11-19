package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

func cloneJSONMap(source map[string]any) map[string]any {
	data, err := json.Marshal(source)
	if err != nil {
		panic(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

func adapterSchema(name string) map[string]any {
	properties := map[string]any{}
	required := []any{}
	add := func(name, kind string, mandatory bool) {
		properties[name] = map[string]any{"type": kind}
		if mandatory {
			required = append(required, name)
		}
	}
	switch strings.ToLower(name) {
	case "read":
		add("file_path", "string", true)
		add("offset", "number", false)
		add("limit", "number", false)
	case "edit":
		add("file_path", "string", true)
		add("old_string", "string", true)
		add("new_string", "string", true)
		add("replace_all", "boolean", false)
		add("sandbox_permissions", "string", false)
		add("justification", "string", false)
	case "skill":
		add("name", "string", true)
	}
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

func adapterTool(protocol wire.Protocol, name string, schema map[string]any) map[string]any {
	description := "Client tool " + name
	switch strings.ToLower(name) {
	case "read":
		description = "Read a UTF-8 text file at file_path and return line-numbered content."
	case "edit":
		description = "Edit an existing UTF-8 text file by replacing literal old_string with new_string."
	case "skill":
		description = "Load the skill with name from the available catalog."
	}
	if protocol == wire.Chat {
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": schema}}
	}
	if protocol == wire.Anthropic {
		return map[string]any{"name": name, "description": description, "input_schema": schema}
	}
	return map[string]any{"type": "function", "name": name, "description": description, "parameters": schema}
}

func TestToolAdapterDefinitions(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			items := []any{}
			for _, name := range []string{"READ", "Edit", "skill"} {
				schema := adapterSchema(name)
				schema["additionalProperties"] = true
				for _, property := range schema["properties"].(map[string]any) {
					property.(map[string]any)["description"] = "CLIENT_ONLY_FIELD_DESCRIPTION"
				}
				items = append(items, adapterTool(protocol, name, schema))
			}
			originalItems := cloneJSONMap(map[string]any{"tools": items})
			payload := map[string]any{"tools": items}
			renames, changed := renameAndCompleteTools(payload, protocol, false, "darwin")
			if !changed {
				t.Fatal("not shaped")
			}
			tools := payload["tools"].([]any)
			if len(tools) != 3 {
				t.Fatalf("got %d declarations, want only 3 bound tools", len(tools))
			}
			for index, name := range []string{"read", "edit", "skill"} {
				tool := tools[index].(map[string]any)
				expected := officialTool(protocol, name, false, "darwin")
				if !reflect.DeepEqual(tool, expected) {
					t.Fatalf("%s declaration is not the exact official contract:\n got %#v\nwant %#v", name, tool, expected)
				}
				schema, actual, _ := toolSchema(tool, protocol)
				if actual != name {
					t.Fatalf("tool %d name = %q", index, actual)
				}
				if properties, ok := schema["properties"].(map[string]any); ok {
					for _, field := range []string{"file_path", "old_string", "new_string", "replace_all", "sandbox_permissions", "justification", "name"} {
						if _, exists := properties[field]; exists {
							t.Fatalf("client property %s leaked into %s", field, name)
						}
					}
				}
			}
			if !reflect.DeepEqual(originalItems, cloneJSONMap(map[string]any{"tools": items})) {
				t.Fatal("client declarations were mutated")
			}
			if name, ok := renames.Wire("READ"); !ok || name != "read" {
				t.Fatalf("READ mapping %q %v", name, ok)
			}
			if name, ok := renames.Client("edit"); !ok || name != "Edit" {
				t.Fatalf("edit mapping %q %v", name, ok)
			}
		})
	}
}

func TestToolAdapterSchemaFallback(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong field type": func(schema map[string]any) {
			schema["properties"].(map[string]any)["file_path"] = map[string]any{"type": "number"}
		},
		"nullable field type": func(schema map[string]any) {
			schema["properties"].(map[string]any)["file_path"] = map[string]any{"type": []any{"string", "null"}}
		},
		"missing required": func(schema map[string]any) { schema["required"] = []any{} },
		"extra required": func(schema map[string]any) {
			schema["properties"].(map[string]any)["tenant"] = map[string]any{"type": "string"}
			schema["required"] = []any{"file_path", "tenant"}
		},
		"wrong optional type": func(schema map[string]any) {
			schema["properties"].(map[string]any)["offset"] = map[string]any{"type": "string"}
		},
		"canonical collision": func(schema map[string]any) {
			schema["properties"].(map[string]any)["path"] = map[string]any{"type": "string"}
		},
		"conditional required": func(schema map[string]any) { schema["allOf"] = []any{map[string]any{"required": []any{"tenant"}}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			schema := adapterSchema("read")
			mutate(schema)
			original := cloneJSONMap(schema)
			payload := map[string]any{"tools": []any{adapterTool(wire.Chat, "read", schema)}}
			renames, _ := renameAndCompleteTools(payload, wire.Chat, false, "darwin")
			tools := payload["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("got %d declarations", len(tools))
			}
			actualSchema, name, _ := toolSchema(tools[0].(map[string]any), wire.Chat)
			if !strings.HasPrefix(name, wire.ClientToolPrefix) || renames.HasArgumentAdapter("read") {
				t.Fatalf("incompatible schema adapted as %s", name)
			}
			if !reflect.DeepEqual(original, cloneJSONMap(actualSchema)) {
				t.Fatal("fallback changed schema")
			}
		})
	}
}

func TestToolAdapterNoUnboundOfficialToolsAndV1(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		payload := map[string]any{}
		renameAndCompleteTools(payload, protocol, false, "darwin")
		if len(payload["tools"].([]any)) != 0 {
			t.Fatal("v2 added tools without executor")
		}
		payload = map[string]any{"tools": []any{adapterTool(protocol, "create_goal", map[string]any{"type": "object", "properties": map[string]any{}})}}
		renameAndCompleteTools(payload, protocol, false, "darwin")
		tools := payload["tools"].([]any)
		if len(tools) != 1 {
			t.Fatal("client-only tool lost or unbound official tools added")
		}
		_, name, _ := toolSchema(tools[0].(map[string]any), protocol)
		if !strings.HasPrefix(name, wire.ClientToolPrefix) {
			t.Fatalf("client-only name %q", name)
		}
		payload = map[string]any{"tools": []any{adapterTool(protocol, "read", adapterSchema("read"))}}
		renames, _ := renameAndCompleteTools(payload, protocol, true, "darwin")
		if len(payload["tools"].([]any)) != len(officialToolNames(true))+1 || renames.HasAdapters() {
			t.Fatal("v1 behavior changed")
		}
	}
}

func assertCanonicalArguments(t *testing.T, value any) {
	t.Helper()
	var arguments map[string]any
	switch value := value.(type) {
	case string:
		if err := json.Unmarshal([]byte(value), &arguments); err != nil {
			t.Fatal(err)
		}
	case map[string]any:
		arguments = value
	default:
		t.Fatalf("unexpected arguments type %T", value)
	}
	if !reflect.DeepEqual(arguments, map[string]any{"path": "fixture.txt"}) {
		t.Fatalf("history is not limited to official properties: %#v", arguments)
	}
}

func TestToolAdapterHistoryAndChoice(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			schema := adapterSchema("read")
			props := schema["properties"].(map[string]any)
			props["sandbox_permissions"] = map[string]any{"type": "string"}
			props["justification"] = map[string]any{"type": "string"}
			payload := map[string]any{"tools": []any{adapterTool(protocol, "Read", schema)}}
			arguments := `{"file_path":"fixture.txt","sandbox_permissions":"workspace-write","justification":"test"}`
			var call, result map[string]any
			switch protocol {
			case wire.Chat:
				call = map[string]any{"name": "Read", "arguments": arguments}
				result = map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"}
				payload["messages"] = []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": call}}}, result}
				payload["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "Read"}}
			case wire.Responses:
				call = map[string]any{"type": "function_call", "name": "Read", "call_id": "call_1", "arguments": arguments}
				result = map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"}
				payload["input"] = []any{call, result}
				payload["tool_choice"] = map[string]any{"type": "function", "name": "Read"}
			case wire.Anthropic:
				call = map[string]any{"type": "tool_use", "name": "Read", "id": "call_1", "input": map[string]any{"file_path": "fixture.txt", "sandbox_permissions": "workspace-write", "justification": "test"}}
				result = map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "result"}
				payload["messages"] = []any{map[string]any{"role": "assistant", "content": []any{call}}, map[string]any{"role": "user", "content": []any{result}}}
				payload["tool_choice"] = map[string]any{"type": "tool", "name": "Read"}
			}
			originalResult := cloneJSONMap(result)
			renames, _ := renameAndCompleteTools(payload, protocol, false, "darwin")
			if call["name"] != "read" {
				t.Fatalf("history name = %v", call["name"])
			}
			key := "arguments"
			if protocol == wire.Anthropic {
				key = "input"
			}
			assertCanonicalArguments(t, call[key])
			choice := payload["tool_choice"].(map[string]any)
			if protocol == wire.Chat {
				choice = choice["function"].(map[string]any)
			}
			if choice["name"] != "read" {
				t.Fatalf("choice name = %v", choice["name"])
			}
			if !reflect.DeepEqual(originalResult, result) {
				t.Fatal("result correlation changed")
			}
			before := cloneJSONMap(payload)
			rewriteToolReferences(payload, protocol, renames)
			if !reflect.DeepEqual(before, cloneJSONMap(payload)) {
				t.Fatal("second conversion changed canonical history")
			}
			if protocol == wire.Responses {
				if len(payload["input"].([]any)) != 2 {
					t.Fatal("history/result dropped")
				}
			} else if len(payload["messages"].([]any)) != 2 {
				t.Fatal("history/result dropped")
			}
		})
	}
}

func TestToolAdapterLegacyFunctions(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses} {
		legacy := map[string]any{"name": "EDIT", "parameters": adapterSchema("edit"), "description": "Replace literal old_string with new_string."}
		function := map[string]any{"name": "EDIT", "arguments": `{"file_path":"fixture.txt","old_string":"old","new_string":"new","replace_all":true}`}
		result := map[string]any{"role": "function", "name": "EDIT", "content": "done"}
		payload := map[string]any{"functions": []any{legacy}, "function_call": map[string]any{"name": "EDIT"}}
		if protocol == wire.Chat {
			payload["messages"] = []any{map[string]any{"role": "assistant", "function_call": function}, result}
		} else {
			payload["input"] = []any{map[string]any{"type": "function_call", "name": "EDIT", "call_id": "legacy_1", "arguments": function["arguments"]}, map[string]any{"type": "function_call_output", "call_id": "legacy_1", "output": "done"}}
		}
		renameAndCompleteTools(payload, protocol, false, "darwin")
		if _, exists := payload["functions"]; exists {
			t.Fatal("legacy declarations retained")
		}
		if len(payload["tools"].([]any)) != 1 {
			t.Fatal("legacy declarations duplicated")
		}
		if payload["function_call"].(map[string]any)["name"] != "edit" {
			t.Fatal("legacy choice not renamed")
		}
		if protocol == wire.Chat {
			if function["name"] != "edit" || result["name"] != "edit" {
				t.Fatal("legacy history/result names differ")
			}
			var arguments map[string]any
			if err := json.Unmarshal([]byte(function["arguments"].(string)), &arguments); err != nil {
				t.Fatal(err)
			}
			if arguments["path"] != "fixture.txt" || arguments["oldString"] != "old" || arguments["newString"] != "new" || arguments["replaceAll"] != true {
				t.Fatalf("legacy arguments %#v", arguments)
			}
		}
	}
}

func TestToolAdapterAllowedTools(t *testing.T) {
	for _, field := range []string{"allowed_tools", "tools"} {
		payload := map[string]any{"tools": []any{adapterTool(wire.Responses, "Skill", adapterSchema("skill"))}, "tool_choice": map[string]any{"type": "allowed_tools", field: []any{map[string]any{"type": "function", "name": "Skill"}}}, "input": []any{map[string]any{"type": "function_call", "name": "Skill", "call_id": "skill_1", "arguments": `{"name":"ponytail"}`}, map[string]any{"type": "function_call_output", "call_id": "skill_1", "output": "instructions"}}}
		renameAndCompleteTools(payload, wire.Responses, false, "darwin")
		choice := payload["tool_choice"].(map[string]any)[field].([]any)[0].(map[string]any)
		if choice["name"] != "skill" {
			t.Fatalf("allowed tool name %v", choice["name"])
		}
		call := payload["input"].([]any)[0].(map[string]any)
		if call["arguments"] != `{"id":"ponytail"}` {
			t.Fatalf("skill args %v", call["arguments"])
		}
	}
}
