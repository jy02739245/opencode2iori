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

func TestOfficialToolReturnsUseClientSchema(t *testing.T) {
	cases := []struct {
		name       string
		schema     map[string]any
		arguments  map[string]any
		wantClient map[string]any
	}{
		{"write", map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []any{"file_path", "content"}, "additionalProperties": false},
			map[string]any{"path": "fixture.txt", "content": "new content", "unexpected": true}, map[string]any{"file_path": "fixture.txt", "content": "new content"}},
		{"read", map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}, "required": []any{"file_path"}, "additionalProperties": false},
			map[string]any{"path": "fixture.txt", "limit": 20}, map[string]any{"file_path": "fixture.txt"}},
		{"grep", map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}, "required": []any{"pattern"}, "additionalProperties": false},
			map[string]any{"pattern": "needle", "literal": true}, map[string]any{"pattern": "needle"}},
	}
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, v1 := range []bool{false, true} {
			for _, tc := range cases {
				originalSchema := cloneJSONMap(tc.schema)
				payload := map[string]any{"tools": []any{adapterTool(protocol, strings.ToUpper(tc.name), tc.schema)}}
				renames, _ := renameAndCompleteTools(payload, protocol, v1, "darwin")
				if !reflect.DeepEqual(payload["tools"], officialToolset(protocol, v1, "darwin")) {
					t.Fatalf("%s v1=%v: client schema leaked upstream", protocol, v1)
				}
				arguments := cloneJSONMap(tc.arguments)
				if v1 && (tc.name == "read" || tc.name == "write") {
					arguments["filePath"] = arguments["path"]
					delete(arguments, "path")
				}
				converted, _ := renames.ToClientArguments(tc.name, arguments)
				if !reflect.DeepEqual(converted, tc.wantClient) {
					t.Fatalf("%s %s v1=%v: returned %#v, want %#v", protocol, tc.name, v1, converted, tc.wantClient)
				}
				if name := renames.Restore(tc.name); name != strings.ToUpper(tc.name) {
					t.Fatalf("client name was not restored: %s", name)
				}
				if !reflect.DeepEqual(tc.schema, originalSchema) {
					t.Fatal("client schema was mutated")
				}
			}
		}
	}
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
			if len(tools) != len(officialToolNames(false)) {
				t.Fatalf("got %d declarations, want the complete official set", len(tools))
			}
			for index, name := range officialToolNames(false) {
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

func TestOfficialDefinitionsOverrideClientSchema(t *testing.T) {
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
			if !reflect.DeepEqual(payload["tools"], officialToolset(wire.Chat, false, "darwin")) {
				t.Fatal("same-name client schema was published instead of the official set")
			}
			if wireName, ok := renames.Wire("read"); !ok || wireName != "read" {
				t.Fatalf("same-name tool was renamed to %q", wireName)
			}
			if !reflect.DeepEqual(original, cloneJSONMap(schema)) {
				t.Fatal("client schema was mutated")
			}
			arguments, _ := renames.ToClientArguments("read", `{"path":"fixture.txt"}`)
			want := `{"file_path":"fixture.txt"}`
			if arguments != want {
				t.Fatalf("returned arguments = %v, want %s", arguments, want)
			}
		})
	}
}

func TestToolAdapterCompletesOfficialToolsAndV1(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, body := range []map[string]any{{}, {"tools": []any{}}} {
			renameAndCompleteTools(body, protocol, false, "darwin")
			if !reflect.DeepEqual(body["tools"], officialToolset(protocol, false, "darwin")) {
				t.Fatalf("%s: missing or empty declarations did not receive the official set", protocol)
			}
		}
		payload := map[string]any{"tools": []any{adapterTool(protocol, "create_goal", map[string]any{"type": "object", "properties": map[string]any{}})}}
		renameAndCompleteTools(payload, protocol, false, "darwin")
		tools := payload["tools"].([]any)
		if len(tools) != len(officialToolNames(false))+1 {
			t.Fatal("client-only tool or full official set missing")
		}
		_, name, _ := toolSchema(tools[len(tools)-1].(map[string]any), protocol)
		if !strings.HasPrefix(name, wire.ClientToolPrefix) {
			t.Fatalf("client-only name %q", name)
		}
		if !reflect.DeepEqual(tools[:len(tools)-1], officialToolset(protocol, false, "darwin")) {
			t.Fatal("official set was not placed before client tools")
		}
		payload = map[string]any{"tools": []any{adapterTool(protocol, "read", adapterSchema("read"))}}
		renames, _ := renameAndCompleteTools(payload, protocol, true, "darwin")
		if !reflect.DeepEqual(payload["tools"], officialToolset(protocol, true, "darwin")) {
			t.Fatal("v1 same-name client tool was not replaced with its official contract")
		}
		arguments, _ := renames.ToClientArguments("read", `{"filePath":"fixture.txt"}`)
		if arguments != `{"file_path":"fixture.txt"}` {
			t.Fatalf("v1 returned arguments = %v", arguments)
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

func TestOfficialToolsPrecedeClientOnlyTools(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, v1 := range []bool{false, true} {
			items := []any{adapterTool(protocol, "create_goal", adapterSchema("create_goal"))}
			for _, name := range officialToolNames(v1) {
				items = append(items, adapterTool(protocol, strings.ToUpper(name), adapterSchema(name)))
			}
			// A repeated declaration must neither publish a duplicate nor replace its adapter.
			items = append(items, adapterTool(protocol, "READ", nil))
			payload := map[string]any{"tools": items}
			renames, _ := renameAndCompleteTools(payload, protocol, v1, "darwin")
			tools := payload["tools"].([]any)
			officials := officialToolset(protocol, v1, "darwin")
			if len(tools) != len(officials)+1 || !reflect.DeepEqual(tools[:len(officials)], officials) {
				t.Fatalf("%s v1=%v: official contracts must come first without duplicates", protocol, v1)
			}
			_, customName, _ := toolSchema(tools[len(officials)].(map[string]any), protocol)
			if renames.Restore(customName) != "create_goal" {
				t.Fatalf("client-only name did not round-trip: %q", customName)
			}
			if renames.Restore("read") != "READ" {
				t.Fatal("official name did not restore the original client name")
			}
			arguments := `{"path":"fixture.txt"}`
			if v1 {
				arguments = `{"filePath":"fixture.txt"}`
			}
			converted, _ := renames.ToClientArguments("read", arguments)
			if converted != `{"file_path":"fixture.txt"}` {
				t.Fatalf("duplicate declaration changed the return adapter: %v", converted)
			}
		}
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
		if len(payload["tools"].([]any)) != len(officialToolNames(false)) {
			t.Fatal("legacy declaration duplicated or official set incomplete")
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

func TestCapabilityAliasBindsExecCommand(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"cmd":               map[string]any{"type": "string"},
			"workdir":           map[string]any{"type": "string"},
			"yield_time_ms":     map[string]any{"type": "number"},
			"max_output_tokens": map[string]any{"type": "number"},
		},
		"required": []any{"cmd"},
	}
	for _, v1 := range []bool{false, true} {
		official := "shell"
		if v1 {
			official = "bash"
		}
		for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
			payload := map[string]any{"tools": []any{adapterTool(protocol, "exec_command", schema)}}
			renames, changed := renameAndCompleteTools(payload, protocol, v1, "darwin")
			if !changed {
				t.Fatalf("%s v1=%v: request was not shaped", protocol, v1)
			}
			hidden := hiddenOfficialTools([]string{"exec_command"}, v1)
			visible := visibleOfficialNames(v1, hidden)
			tools := payload["tools"].([]any)
			if len(tools) != len(visible)+1 {
				t.Fatalf("%s v1=%v: got %d declarations, want %d official plus the client alias", protocol, v1, len(tools), len(visible))
			}
			for index, name := range visible {
				if !reflect.DeepEqual(tools[index], officialTool(protocol, name, v1, "darwin")) {
					t.Fatalf("%s v1=%v: official %q declaration drifted", protocol, v1, name)
				}
			}
			if !v1 {
				for _, name := range []string{"read", "write", "edit", "patch"} {
					if !hidden[name] {
						t.Fatalf("%s: official %q must be hidden for a command client", protocol, name)
					}
				}
			}
			alias, ok := renames.Wire("exec_command")
			if !ok || alias == official || alias == "exec_command" {
				t.Fatalf("%s v1=%v: client tool wire name = %q, %v; want an opaque alias", protocol, v1, alias, ok)
			}
			if restored := renames.Restore(alias); restored != "exec_command" {
				t.Fatalf("%s v1=%v: alias did not restore: %q", protocol, v1, restored)
			}
			if !renames.Keep(official) {
				t.Fatalf("%s v1=%v: official %q must stay executable", protocol, v1, official)
			}
			if clientName, _ := renames.Official(official); clientName != "exec_command" {
				t.Fatalf("%s v1=%v: official name bound to %q, want exec_command", protocol, v1, clientName)
			}
			if client, _ := renames.Resolve(official); client != "exec_command" {
				t.Fatalf("%s v1=%v: official name restored to %q, want exec_command", protocol, v1, client)
			}
			// The official spelling carries the official argument names.
			converted, adapted := renames.ToClientArguments(official, map[string]any{
				"command": "ls", "workdir": "/tmp", "timeout": 1000, "background": true,
			})
			if !adapted {
				t.Fatalf("%s v1=%v: official arguments were not adapted", protocol, v1)
			}
			want := map[string]any{"cmd": "ls", "workdir": "/tmp"}
			if !reflect.DeepEqual(converted, want) {
				t.Fatalf("%s v1=%v: arguments = %#v, want %#v", protocol, v1, converted, want)
			}
			// The client's own alias keeps the client's own arguments.
			if converted, adapted := renames.ToClientArguments(alias, map[string]any{"cmd": "ls", "workdir": "/tmp"}); adapted {
				t.Fatalf("%s v1=%v: client alias arguments were rewritten to %#v", protocol, v1, converted)
			}
			// History replays under the client alias with the client's arguments.
			wireArgs, changedArgs := renames.ToWireArguments("exec_command", map[string]any{"cmd": "pwd", "workdir": "/tmp"})
			if changedArgs || !reflect.DeepEqual(wireArgs, map[string]any{"cmd": "pwd", "workdir": "/tmp"}) {
				t.Fatalf("%s v1=%v: history arguments = %#v, changed=%v", protocol, v1, wireArgs, changedArgs)
			}
		}
	}
}

func TestUnexecutableOfficialCallIsDropped(t *testing.T) {
	payload := map[string]any{"tools": []any{adapterTool(wire.Chat, "exec_command", map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"},
	})}}
	renames, _ := renameAndCompleteTools(payload, wire.Chat, false, "darwin")
	if !renames.Enforcing() {
		t.Fatal("shaped request must enforce the client tool vocabulary")
	}
	if renames.Keep("read") || renames.Keep("write") || renames.Keep("patch") {
		t.Fatal("an official tool the client cannot execute must not be kept")
	}
	if !renames.Keep("shell") {
		t.Fatal("the bound official shell must stay executable")
	}
}

func TestHiddenOfficialTools(t *testing.T) {
	hidden := hiddenOfficialTools([]string{"exec_command"}, false)
	for _, name := range []string{"read", "write", "edit", "patch"} {
		if !hidden[name] {
			t.Fatalf("official %q must be hidden for a command-only client", name)
		}
	}
	if hidden["shell"] || hidden["glob"] || hidden["grep"] {
		t.Fatalf("shell and search tools must stay published: %#v", hidden)
	}
	hidden = hiddenOfficialTools([]string{"exec_command", "Read"}, false)
	if hidden["read"] {
		t.Fatal("an official tool the client declares must stay published")
	}
	if !hidden["edit"] {
		t.Fatal("undeclared file tools must stay hidden")
	}
	if hiddenOfficialTools([]string{"Read", "Edit", "Write"}, false) != nil {
		t.Fatal("a client without a command tool keeps the full official set")
	}
	if hiddenOfficialTools([]string{"exec_command"}, true) != nil {
		t.Fatal("the 1.x lane keeps the full official set")
	}
}
