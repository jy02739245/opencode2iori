package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"opencode2api/internal/jsonutil"
)

// dropTestRenames declares one client tool with a wire alias and enforces the
// shaped-request policy: a call that maps to nothing must be dropped.
func dropTestRenames(t *testing.T) *ToolRenames {
	t.Helper()
	r := NewToolRenames()
	r.Add("exec_command", "mcp_aa6e99885a69")
	r.Enforce()
	return r
}

func TestKeepMapsDeclaredTools(t *testing.T) {
	r := dropTestRenames(t)
	for _, name := range []string{"mcp_aa6e99885a69", "exec_command"} {
		if !r.Keep(name) {
			t.Fatalf("Keep(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"shell", "read", "mcp_other"} {
		if r.Keep(name) {
			t.Fatalf("Keep(%q) = true, want false", name)
		}
	}
	off := NewToolRenames()
	off.Add("exec_command", "mcp_aa6e99885a69")
	if !off.Keep("shell") {
		t.Fatal("without enforcement every name must be kept")
	}
}

func TestDropUnexecutableChatResponse(t *testing.T) {
	r := dropTestRenames(t)
	body := `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}},` +
		`{"id":"c2","type":"function","function":{"name":"mcp_aa6e99885a69","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`
	out := RenameResponseTool([]byte(body), Chat, r)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	choice := jsonutil.SliceAt(payload, "choices")[0].(map[string]any)
	calls := jsonutil.SliceAt(choice, "message", "tool_calls")
	if len(calls) != 1 {
		t.Fatalf("kept %d calls, want 1: %s", len(calls), out)
	}
	call := calls[0].(map[string]any)
	if name := jsonutil.StringAt(call, "function", "name"); name != "exec_command" {
		t.Fatalf("kept call name = %q, want exec_command", name)
	}
	if reason := jsonutil.StringAt(choice, "finish_reason"); reason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", reason)
	}
}

func TestDropUnexecutableChatResponseDowngradesFinish(t *testing.T) {
	r := dropTestRenames(t)
	body := `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"shell","arguments":"{}"}}]}}]}`
	out := RenameResponseTool([]byte(body), Chat, r)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	choice := jsonutil.SliceAt(payload, "choices")[0].(map[string]any)
	if calls := jsonutil.SliceAt(choice, "message", "tool_calls"); len(calls) != 0 {
		t.Fatalf("dropped call survived: %s", out)
	}
	if reason := jsonutil.StringAt(choice, "finish_reason"); reason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", reason)
	}
}

func TestDropUnexecutableResponsesResponse(t *testing.T) {
	r := dropTestRenames(t)
	body := `{"output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call","id":"fc_2","call_id":"c2","name":"mcp_aa6e99885a69","arguments":"{}"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`
	out := RenameResponseTool([]byte(body), Responses, r)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	items := jsonutil.SliceAt(payload, "output")
	if len(items) != 2 {
		t.Fatalf("kept %d items, want 2: %s", len(items), out)
	}
	if name := jsonutil.StringAt(items[0].(map[string]any), "name"); name != "exec_command" {
		t.Fatalf("kept call name = %q, want exec_command", name)
	}
}

func TestDropUnexecutableAnthropicResponse(t *testing.T) {
	r := dropTestRenames(t)
	body := `{"stop_reason":"tool_use","content":[` +
		`{"type":"tool_use","id":"tu_1","name":"read","input":{}},` +
		`{"type":"text","text":"done"}]}`
	out := RenameResponseTool([]byte(body), Anthropic, r)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	blocks := jsonutil.SliceAt(payload, "content")
	if len(blocks) != 1 {
		t.Fatalf("kept %d blocks, want 1: %s", len(blocks), out)
	}
	if reason := jsonutil.StringAt(payload, "stop_reason"); reason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", reason)
	}
}

func TestStreamDropsUnexecutableChatCall(t *testing.T) {
	input := streamTestChat(
		streamTestChoice(0, nil, streamTestCall(0, "c1", "shell", `{"command":"ls"}`)),
		streamTestChoice(0, nil, streamTestCall(0, "", "", `}`)),
	) + streamTestChat(streamTestChoice(0, "tool_calls")) + "data: [DONE]\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Chat, dropTestRenames(t)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, `"shell"`) {
		t.Fatalf("dropped call name leaked: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason was not downgraded: %s", text)
	}
}

func TestStreamKeepsExecutableChatCall(t *testing.T) {
	input := streamTestChat(
		streamTestChoice(0, nil,
			streamTestCall(0, "c1", "shell", `{"command":"ls"}`),
			streamTestCall(1, "c2", "mcp_aa6e99885a69", `{"cmd":"pwd"}`)),
	) + streamTestChat(streamTestChoice(0, "tool_calls")) + "data: [DONE]\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Chat, dropTestRenames(t)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, `"shell"`) {
		t.Fatalf("dropped call name leaked: %s", text)
	}
	if !strings.Contains(text, `"exec_command"`) {
		t.Fatalf("kept call was not restored: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should stay a tool call: %s", text)
	}
}

func TestStreamDropsUnexecutableResponsesItem(t *testing.T) {
	input := streamTestEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0, "item_id": "fc_1",
		"item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "c1", "name": "read", "arguments": ""},
	}) + streamTestEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "fc_1", "output_index": 0, "delta": `{"path":"a"}`,
	}) + streamTestEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{"output": []any{
			map[string]any{"type": "function_call", "id": "fc_1", "call_id": "c1", "name": "read", "arguments": "{}"},
			map[string]any{"type": "function_call", "id": "fc_2", "call_id": "c2", "name": "mcp_aa6e99885a69", "arguments": "{}"},
		}},
	}) + "data: [DONE]\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Responses, dropTestRenames(t)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, `"read"`) || strings.Contains(text, "fc_1") {
		t.Fatalf("dropped item leaked: %s", text)
	}
	if !strings.Contains(text, `"exec_command"`) {
		t.Fatalf("kept item was not restored: %s", text)
	}
}

func TestStreamDropsUnexecutableAnthropicBlock(t *testing.T) {
	input := streamTestEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": "tu_1", "name": "read", "input": map[string]any{}},
	}) + streamTestEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}) + streamTestEvent("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"},
	}) + "data: [DONE]\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Anthropic, dropTestRenames(t)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, `"tool_use"`) {
		t.Fatalf("dropped block leaked: %s", text)
	}
	if !strings.Contains(text, `"end_turn"`) {
		t.Fatalf("stop_reason was not downgraded: %s", text)
	}
}

func TestEnforceWithoutDeclaredToolsDropsCalls(t *testing.T) {
	r := NewToolRenames()
	r.Enforce()
	body := `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"shell","arguments":"{}"}}]}}]}`
	out := RenameResponseTool([]byte(body), Chat, r)
	if strings.Contains(string(out), `"shell"`) {
		t.Fatalf("tool call survived without a declared tool: %s", out)
	}
	if !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason was not downgraded: %s", out)
	}
	input := streamTestChat(streamTestChoice(0, nil, streamTestCall(0, "c1", "shell", `{}`))) +
		streamTestChat(streamTestChoice(0, "tool_calls")) + "data: [DONE]\n\n"
	streamed, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Chat, r))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(streamed), `"shell"`) {
		t.Fatalf("streamed tool call survived: %s", streamed)
	}
}
