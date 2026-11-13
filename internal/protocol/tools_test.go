package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// shellRenames builds the rename set a shaped request produces when the client
// declared exec_command and the gateway bound the official shell tool to it.
func shellRenames() *ToolRenames {
	renames := NewToolRenames()
	renames.Add("exec_command", ClientToolWireName("exec_command", map[string]bool{}))
	renames.BindOfficial("shell", "exec_command")
	renames.BindOfficialArgs("shell", map[string]string{
		"command":    "cmd",
		"workdir":    "workdir",
		"timeout":    "",
		"background": "",
	})
	return renames
}

func TestRemapArguments(t *testing.T) {
	mapping := map[string]string{
		"command":    "cmd",
		"workdir":    "workdir",
		"timeout":    "",
		"background": "",
	}
	got := RemapArguments(`{"command":"pwd","workdir":"/tmp","timeout":5000,"background":false}`, mapping)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("arguments %q is not JSON: %v", got, err)
	}
	if decoded["cmd"] != "pwd" {
		t.Fatalf("cmd = %v; want pwd", decoded["cmd"])
	}
	if decoded["workdir"] != "/tmp" {
		t.Fatalf("workdir = %v; want /tmp", decoded["workdir"])
	}
	for _, dropped := range []string{"command", "timeout", "background"} {
		if _, ok := decoded[dropped]; ok {
			t.Fatalf("arguments kept %q: %v", dropped, decoded)
		}
	}
}

func TestRemapArgumentsLeavesOtherJSONAlone(t *testing.T) {
	mapping := map[string]string{"command": "cmd"}
	if got := RemapArguments(`{"cmd":"pwd"}`, mapping); got != `{"cmd":"pwd"}` {
		t.Fatalf("arguments without an official name changed: %q", got)
	}
	if got := RemapArguments(`not json`, mapping); got != `not json` {
		t.Fatalf("non-JSON arguments changed: %q", got)
	}
}

func TestRenameResponseToolRestoresExecCommand(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\",\"workdir\":\"/tmp\"}"}}]},"finish_reason":"tool_calls"}]}`)
	out := RenameResponseTool(body, Chat, shellRenames())
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	call := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "exec_command" {
		t.Fatalf("name = %v; want exec_command", function["name"])
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(function["arguments"].(string)), &arguments); err != nil {
		t.Fatalf("arguments %v: %v", function["arguments"], err)
	}
	if arguments["cmd"] != "pwd" || arguments["workdir"] != "/tmp" {
		t.Fatalf("arguments = %v; want cmd and workdir", arguments)
	}
	if _, ok := arguments["command"]; ok {
		t.Fatalf("arguments kept command: %v", arguments)
	}
}

func TestRenameStreamReaderRestoresExecCommand(t *testing.T) {
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"comm"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"pwd\"}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	stream := strings.Join(frames, "\n\n") + "\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(stream), Chat, shellRenames()))
	if err != nil {
		t.Fatal(err)
	}
	name, arguments := collectStreamCalls(t, string(out))
	if name != "exec_command" {
		t.Fatalf("name = %q; want exec_command", name)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatalf("arguments %q: %v", arguments, err)
	}
	if decoded["cmd"] != "pwd" {
		t.Fatalf("arguments = %v; want cmd pwd", decoded)
	}
}

// collectStreamCalls reads every tool call out of a rewritten Chat SSE stream.
func collectStreamCalls(t *testing.T, stream string) (string, string) {
	t.Helper()
	name := ""
	var arguments strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		if raw == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		for _, rawChoice := range payload["choices"].([]any) {
			choice, _ := rawChoice.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if fragment, ok := function["name"].(string); ok && fragment != "" {
					name = fragment
				}
				if fragment, ok := function["arguments"].(string); ok {
					arguments.WriteString(fragment)
				}
			}
		}
	}
	return name, arguments.String()
}

// TestResolveRebuildsMangledWireName covers a model that imitates a wire name
// from history and drops one letter.
func TestResolveRebuildsMangledWireName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	digest := strings.TrimPrefix(wireName, ClientToolPrefix)
	for _, mangled := range []string{"clnt_" + digest, "clit_" + digest} {
		client, changed := renames.Resolve(mangled)
		if client != "exec_command" || !changed {
			t.Fatalf("Resolve(%q) = (%q, %v); want (exec_command, true)", mangled, client, changed)
		}
	}
}

// TestRenameResponseToolRebuildsMangledWireName keeps the argument rewrite out
// of the way: a mangled name carries client arguments already.
func TestRenameResponseToolRebuildsMangledWireName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	digest := strings.TrimPrefix(wireName, ClientToolPrefix)
	body := []byte(`{"choices":[{"index":0,"message":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"clnt_` + digest + `","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`)
	out := RenameResponseTool(body, Chat, renames)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	function := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "exec_command" {
		t.Fatalf("name = %v; want exec_command", function["name"])
	}
	if function["arguments"] != `{"cmd":"pwd"}` {
		t.Fatalf("arguments = %v; want unchanged cmd object", function["arguments"])
	}
}

// TestResolveLeavesUnknownMangledNameAlone pins the rebuild to registered wire
// names: a mangled-looking name that matches nothing stays as it arrived.
func TestResolveLeavesUnknownMangledNameAlone(t *testing.T) {
	renames := shellRenames()
	for _, name := range []string{"clnt_deadbeef0000", "clit_deadbeef0000"} {
		client, changed := renames.Resolve(name)
		if client != name || changed {
			t.Fatalf("Resolve(%q) = (%q, %v); want (%q, false)", name, client, changed, name)
		}
	}
}

// TestResolvePrefersDeclaredLiteralName keeps a client tool whose real name
// looks mangled from being rewritten into another tool.
func TestResolvePrefersDeclaredLiteralName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	literal := "clnt_" + strings.TrimPrefix(wireName, ClientToolPrefix)
	renames.Add(literal, ClientToolWireName(literal, map[string]bool{}))
	client, changed := renames.Resolve(literal)
	if client != literal || changed {
		t.Fatalf("Resolve(%q) = (%q, %v); want (%q, false)", literal, client, changed, literal)
	}
}

// TestRenameStreamReaderRebuildsMangledWireName covers the stream path when the
// mangled name arrives in one frame.
func TestRenameStreamReaderRebuildsMangledWireName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	digest := strings.TrimPrefix(wireName, ClientToolPrefix)
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"clnt_` + digest + `","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n"), Chat, renames))
	if err != nil {
		t.Fatal(err)
	}
	name, arguments := collectStreamCalls(t, string(out))
	if name != "exec_command" {
		t.Fatalf("name = %q; want exec_command", name)
	}
	if arguments != `{"cmd":"pwd"}` {
		t.Fatalf("arguments = %q; want unchanged cmd object", arguments)
	}
}
