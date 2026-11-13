package gateway

import (
	"io"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

// TestShapeBindsExecCommandArguments covers the codex 0.167 case: the client
// declares exec_command, the gateway binds the official shell tool to it, and
// the argument names the official schema uses must be translated to the ones
// exec_command declares.
func TestShapeBindsExecCommandArguments(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "exec_command",
					"description": "Run a command",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":     map[string]any{"type": "string"},
							"workdir": map[string]any{"type": "string"},
						},
						"required": []any{"cmd"},
					},
				},
			},
		},
	}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false)
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	client, bound := renames.Official("shell")
	if !bound || client != "exec_command" {
		t.Fatalf("shell bound to %q (bound=%v); want exec_command", client, bound)
	}
	mapping, ok := renames.OfficialArgs("shell")
	if !ok {
		t.Fatal("shell argument mapping missing")
	}
	if mapping["command"] != "cmd" {
		t.Fatalf("command maps to %q; want cmd", mapping["command"])
	}
	if mapping["workdir"] != "workdir" {
		t.Fatalf("workdir maps to %q; want workdir", mapping["workdir"])
	}
	for _, dropped := range []string{"timeout", "background"} {
		value, known := mapping[dropped]
		if !known || value != "" {
			t.Fatalf("%s maps to %q (known=%v); want dropped", dropped, value, known)
		}
	}
}

// TestShapeLeavesUnboundClientToolsAlone checks the mapping is only recorded
// for a client tool the official name actually bound to.
func TestShapeLeavesUnboundClientToolsAlone(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "read_file",
					"description": "Read a file",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"path": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false)
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	if _, bound := renames.Official("shell"); bound {
		t.Fatal("shell bound to a client tool that is not exec_command")
	}
	if _, ok := renames.OfficialArgs("shell"); ok {
		t.Fatal("shell argument mapping recorded without a binding")
	}
}

// TestShapedStreamRestoresExecCommandArguments runs the whole path: a shaped
// request declares exec_command, the upstream answers with the official shell
// tool, and the stream rewriter must hand the client exec_command with cmd.
func TestShapedStreamRestoresExecCommandArguments(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "exec_command",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":     map[string]any{"type": "string"},
							"workdir": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false)
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"comm"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"pwd\",\"timeout\":5000}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	out, err := io.ReadAll(wire.RenameStreamReader(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n"), wire.Chat, renames))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, `"shell"`) {
		t.Fatalf("stream still carries the official name: %s", text)
	}
	if !strings.Contains(text, `"name":"exec_command"`) {
		t.Fatalf("stream lacks exec_command: %s", text)
	}
	if !strings.Contains(text, `\"cmd\":\"pwd\"`) {
		t.Fatalf("stream lacks cmd: %s", text)
	}
	if strings.Contains(text, `"timeout"`) {
		t.Fatalf("stream kept the dropped timeout argument: %s", text)
	}
}
