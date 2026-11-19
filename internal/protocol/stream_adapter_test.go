package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"opencode2api/internal/jsonutil"
)

func streamTestToolAdapters(t *testing.T) *ToolRenames {
	t.Helper()
	r := NewToolRenames()
	if !r.BindAdapter("read", "read", map[string]string{"path": "file_path"}) ||
		!r.BindAdapter("client_edit", "edit", map[string]string{"oldString": "old_string"}) {
		t.Fatal("could not register tool adapters")
	}
	r.Add("glob", "mcp_glob")
	return r
}

func streamTestEvent(name string, value any) string {
	if name != "" {
		name = "event: " + name + "\n"
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return name + "data: " + string(data) + "\n\n"
}

func streamTestChoice(index int, finish any, calls ...any) map[string]any {
	return map[string]any{"index": index, "finish_reason": finish, "delta": map[string]any{"tool_calls": calls}}
}

func streamTestChat(choices ...any) string {
	return streamTestEvent("", map[string]any{"choices": choices})
}

func streamTestCall(index int, id, name, args string) map[string]any {
	call := map[string]any{"index": index}
	function := map[string]any{}
	if id != "" {
		call["id"], call["type"] = id, "function"
	}
	if name != "" {
		function["name"] = name
	}
	if args != "" {
		function["arguments"] = args
	}
	call["function"] = function
	return call
}

func TestStreamAdapterChatInterleaved(t *testing.T) {
	read := func(name, args string) map[string]any { return streamTestCall(0, "", name, args) }
	edit := func(name, args string) map[string]any { return streamTestCall(1, "", name, args) }
	rawOnly := streamTestChat(streamTestChoice(0, nil, streamTestCall(2, "", "", `th":"untouched"}`)))
	textOnly := "data: {\"choices\":[{\"index\":0, \"delta\":{\"content\":\"path\"}}]}\n\n"
	input := streamTestChat(streamTestChoice(0, nil, streamTestCall(0, "r0", "re", `{"pa`), streamTestCall(1, "e1", "edit", `{"old`), streamTestCall(2, "g2", "mcp_glob", `{"pa`)),
		streamTestChoice(1, nil, streamTestCall(0, "r1", "read", `{"path":"other.txt"}`))) +
		textOnly + streamTestChat(streamTestChoice(0, nil, edit("", `String":"path", "nested":{"oldString":"path"}}`))) +
		streamTestChat(streamTestChoice(0, nil, read("ad", `th":"literal \`))) + rawOnly +
		streamTestChat(streamTestChoice(0, "tool_calls", read("", `"path\" and }", "nested":[{"path":9007199254740993}]}`)), streamTestChoice(1, "tool_calls")) + "data: [DONE]\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Chat, streamTestToolAdapters(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{textOnly, rawOnly} {
		if !strings.Contains(string(out), raw) {
			t.Fatalf("expected unchanged frame %q", raw)
		}
	}
	type callKey struct{ choice, index string }
	type call struct{ id, name, args string }
	got := map[callKey]call{}
	var finishes, done int
	if err := readSSE(strings.NewReader(string(out)), func(_, data string) error {
		if data == "[DONE]" {
			done++
			return nil
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return err
		}
		for _, entry := range jsonutil.SliceAt(frame, "choices") {
			choice := entry.(map[string]any)
			for _, rawCall := range jsonutil.SliceAt(choice, "delta", "tool_calls") {
				c := rawCall.(map[string]any)
				key := callKey{jsonutil.FirstString(jsonNumberString(choice["index"])), jsonutil.FirstString(jsonNumberString(c["index"]))}
				v := got[key]
				v.id += jsonutil.StringAt(c, "id")
				v.name += jsonutil.StringAt(c, "function", "name")
				v.args += jsonutil.StringAt(c, "function", "arguments")
				got[key] = v
			}
			if jsonutil.StringAt(choice, "finish_reason") == "tool_calls" {
				finishes++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[callKey]call{
		{"0", "0"}: {"r0", "read", `{"file_path":"literal \"path\" and }","nested":[{"path":9007199254740993}]}`},
		{"0", "1"}: {"e1", "client_edit", `{"nested":{"oldString":"path"},"old_string":"path"}`},
		{"0", "2"}: {"g2", "glob", `{"path":"untouched"}`},
		{"1", "0"}: {"r1", "read", `{"file_path":"other.txt"}`},
	}
	if len(got) != len(want) || finishes != 2 || done != 1 {
		t.Fatalf("unexpected stream lifecycle: calls=%#v finishes=%d done=%d", got, finishes, done)
	}
	for key, val := range want {
		if got[key] != val {
			t.Errorf("call %v = %#v, want %#v", key, got[key], val)
		}
	}
}

func jsonNumberString(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestStreamAdapterChatLegacyFunctionCall(t *testing.T) {
	legacy := func(fn map[string]any, finish any) string {
		return streamTestChat(map[string]any{"index": 0, "finish_reason": finish, "delta": map[string]any{"function_call": fn}})
	}
	input := legacy(map[string]any{"name": "ed", "arguments": `{"old`}, nil) +
		legacy(map[string]any{"name": "it", "arguments": `String":"`}, nil) +
		legacy(map[string]any{"arguments": `hello"}`}, "function_call") + "data: [DONE]\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Chat, streamTestToolAdapters(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := legacy(map[string]any{}, nil) +
		legacy(map[string]any{"name": "client_edit", "arguments": ""}, nil) +
		legacy(map[string]any{"arguments": `{"old_string":"hello"}`}, "function_call") + "data: [DONE]\n\n"
	if string(out) != want {
		t.Fatalf("unexpected legacy Chat frames:\n%s", out)
	}
}

func TestStreamAdapterChatTermination(t *testing.T) {
	for _, tt := range []struct {
		name, terminal string
	}{
		{"EOF", ""},
		{"DONE", "data: [DONE]\n\n"},
		{"finish_reason", streamTestChat(streamTestChoice(0, "tool_calls"))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, complete := range []bool{false, true} {
				input := streamTestChat(streamTestChoice(0, nil, streamTestCall(0, "id", "read", `{"pa`)))
				if complete {
					input += streamTestChat(streamTestChoice(0, nil, streamTestCall(0, "", "", `th":"a.txt"}`)))
				}
				out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input+tt.terminal), Chat, streamTestToolAdapters(t)))
				if !complete {
					if !errors.Is(err, errStreamToolArguments) || (tt.terminal != "" && strings.Contains(string(out), tt.terminal)) {
						t.Fatalf("incomplete arguments should stop stream before terminal event, got %q (%v)", out, err)
					}
					continue
				}
				if err != nil || !strings.Contains(string(out), `\"file_path\":\"a.txt\"`) || !strings.HasSuffix(string(out), tt.terminal) {
					t.Fatalf("complete arguments should survive %s, got %q (%v)", tt.name, out, err)
				}
			}
		})
	}
}

func TestStreamAdapterResponses(t *testing.T) {
	item := func(id, name, args string) map[string]any {
		return map[string]any{"type": "function_call", "id": id, "call_id": "call_" + id, "name": name, "arguments": args}
	}
	added := func(seq int, id, name string) string {
		return streamTestEvent("response.output_item.added", map[string]any{"sequence_number": seq, "item": item(id, name, "")})
	}
	delta := func(seq int, id, fragment string) string {
		return streamTestEvent("response.function_call_arguments.delta", map[string]any{"sequence_number": seq, "item_id": id, "delta": fragment})
	}
	readArgs := `{"path":"notes", "nested":{"path":"literal"}}`
	editArgs := `{"oldString":"x"}`
	input := added(0, "r", "read") + added(1, "e", "edit") +
		delta(2, "e", `{"old`) + delta(3, "r", `{"pa`) + delta(4, "e", `String":"x"}`) + delta(5, "r", `th":"notes", "nested":{"path":"literal"}}`) +
		streamTestEvent("response.function_call_arguments.done", map[string]any{"sequence_number": 6, "item_id": "r", "arguments": readArgs}) +
		streamTestEvent("response.output_item.done", map[string]any{"sequence_number": 7, "item": item("e", "edit", editArgs)}) +
		streamTestEvent("response.completed", map[string]any{"sequence_number": 8, "response": map[string]any{"output": []any{item("r", "read", readArgs), item("e", "edit", editArgs)}}})
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Responses, streamTestToolAdapters(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantDeltas := map[string]string{"e": `{"old_string":"x"}`, "r": `{"file_path":"notes","nested":{"path":"literal"}}`}
	deltas := map[string]string{}
	i := 0
	if err := readSSE(strings.NewReader(string(out)), func(event, data string) error {
		var frame map[string]any
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return err
		}
		if frame["sequence_number"] != float64(i) {
			t.Fatalf("event %d reordered: %s", i, data)
		}
		i++
		switch event {
		case "response.function_call_arguments.delta":
			deltas[jsonutil.StringAt(frame, "item_id")] += jsonutil.StringAt(frame, "delta")
		case "response.function_call_arguments.done":
			if got := jsonutil.StringAt(frame, "arguments"); got != wantDeltas["r"] {
				t.Fatalf("done arguments = %s", got)
			}
		case "response.output_item.added", "response.output_item.done":
			if jsonutil.StringAt(frame, "item", "id") == "e" && (jsonutil.StringAt(frame, "item", "call_id") != "call_e" || jsonutil.StringAt(frame, "item", "name") != "client_edit") {
				t.Fatalf("edit item was not correctly adapted: %s", data)
			}
		case "response.completed":
			for _, raw := range jsonutil.SliceAt(frame, "response", "output") {
				item := raw.(map[string]any)
				if got := jsonutil.StringAt(item, "arguments"); got != wantDeltas[jsonutil.StringAt(item, "id")] {
					t.Fatalf("completed arguments = %s", got)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if i != 9 || deltas["e"] != wantDeltas["e"] || deltas["r"] != wantDeltas["r"] {
		t.Fatalf("Responses frame count = %d, deltas = %v", i, deltas)
	}
}

func TestStreamAdapterAnthropic(t *testing.T) {
	start := func(index int, id, name string, input map[string]any) string {
		return streamTestEvent("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}})
	}
	delta := func(index int, partial string) string {
		return streamTestEvent("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": partial}})
	}
	stop := streamTestEvent("content_block_stop", map[string]any{"index": 0})
	open := start(0, "tool_r", "read", map[string]any{}) + delta(0, `{"pa`)
	input := open + delta(0, `th":"a.txt","nested":{"path":"path"}}`) + stop +
		start(1, "tool_e", "edit", map[string]any{"oldString": "hello", "n": json.Number("9007199254740993")}) +
		streamTestEvent("content_block_stop", map[string]any{"index": 1}) + streamTestEvent("message_stop", map[string]any{})
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(input), Anthropic, streamTestToolAdapters(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		start(0, "tool_r", "read", map[string]any{}),
		delta(0, ""), delta(0, `{"file_path":"a.txt","nested":{"path":"path"}}`), stop,
		start(1, "tool_e", "client_edit", map[string]any{"old_string": "hello", "n": json.Number("9007199254740993")}),
		streamTestEvent("content_block_stop", map[string]any{"index": 1}), streamTestEvent("message_stop", map[string]any{}),
	}
	if string(out) != strings.Join(want, "") {
		t.Fatalf("unexpected Anthropic events:\n%s", out)
	}
	if _, err = io.ReadAll(RenameStreamReader(strings.NewReader(open+stop), Anthropic, streamTestToolAdapters(t))); !errors.Is(err, errStreamToolArguments) {
		t.Fatalf("truncated block error = %v", err)
	}
}
