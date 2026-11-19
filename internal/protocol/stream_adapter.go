package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"opencode2api/internal/jsonutil"
)

var errStreamToolArguments = errors.New("upstream stream contains invalid or incomplete tool arguments")

// streamArgumentBuffer tracks the boundary of one JSON object in linear time.
// Later whitespace is allowed, but any second value is a stream error.
type streamArgumentBuffer struct {
	wireName string
	text     strings.Builder
	depth    int
	quoted   bool
	escaped  bool
	complete bool
}

// argumentFragment emits the converted object on the delta that closes it.
// Earlier deltas keep their IDs and sequence numbers with an empty fragment.
func (renamer *streamToolRenamer) argumentFragment(state *streamArgumentBuffer, fragment string) (string, error) {
	if state.complete {
		if strings.Trim(fragment, " \r\n\t") != "" {
			return "", errStreamToolArguments
		}
		return fragment, nil
	}
	if len(fragment) > maxSSEFrameBytes-renamer.argumentBytes {
		return "", errSSEFrameTooLarge
	}
	state.text.WriteString(fragment)
	renamer.argumentBytes += len(fragment)
	for i := 0; i < len(fragment); i++ {
		ch := fragment[i]
		if state.quoted {
			if state.escaped {
				state.escaped = false
			} else if ch == '\\' {
				state.escaped = true
			} else if ch == '"' {
				state.quoted = false
			}
			continue
		}
		if state.depth == 0 {
			switch ch {
			case ' ', '\n', '\t', '\r':
				continue
			case '{':
				state.depth = 1
				continue
			default:
				return "", errStreamToolArguments
			}
		}
		switch ch {
		case '"':
			state.quoted = true
		case '{', '[':
			state.depth++
		case '}', ']':
			state.depth--
			if state.depth == 0 {
				text := state.text.String()
				if !json.Valid([]byte(text)) {
					return "", errStreamToolArguments
				}
				converted, _ := renamer.renames.ToClientArguments(state.wireName, text)
				renamer.argumentBytes -= state.text.Len()
				state.text.Reset()
				state.complete = true
				return converted.(string), nil
			}
		}
	}
	return "", nil
}

func (renamer *streamToolRenamer) adaptFragment(state *streamArgumentBuffer, frame map[string]any, field string) (bool, error) {
	fragment, ok := frame[field].(string)
	if !ok {
		return false, errStreamToolArguments
	}
	converted, err := renamer.argumentFragment(state, fragment)
	if err != nil || converted == fragment {
		return false, err
	}
	frame[field] = converted
	return true, nil
}

func (renamer *streamToolRenamer) adaptChatArguments(state *streamToolRenameState, function map[string]any) (bool, error) {
	arguments, exists := function["arguments"]
	if !exists || !renamer.renames.HasArgumentAdapter(state.pending) {
		return false, nil
	}
	if state.adapted == nil {
		state.adapted = &streamArgumentBuffer{wireName: state.pending}
	}
	if object, ok := arguments.(map[string]any); ok && object != nil && !state.adapted.complete && state.adapted.text.Len() == 0 {
		state.adapted.complete = true
		converted, changed := renamer.renames.ToClientArguments(state.pending, object)
		if changed {
			function["arguments"] = converted
		}
		return changed, nil
	}
	return renamer.adaptFragment(state.adapted, function, "arguments")
}

// The Responses and Anthropic delta events identify the call without its name.
// Record the original wire name when the corresponding item or block opens.
func (renamer *streamToolRenamer) argumentState(key, wireName string) *streamArgumentBuffer {
	if !renamer.renames.HasArgumentAdapter(wireName) {
		return nil
	}
	if state := renamer.arguments[key]; state != nil {
		return state
	}
	state := &streamArgumentBuffer{wireName: wireName}
	if renamer.arguments == nil {
		renamer.arguments = make(map[string]*streamArgumentBuffer)
	}
	renamer.arguments[key] = state
	return state
}

func (renamer *streamToolRenamer) rewriteArgumentFrame(eventName, raw string) ([]byte, error) {
	// Tool argument and terminal events may have no name to trigger the normal fast path.
	if !mayCarryToolName([]byte(raw)) {
		switch eventName {
		case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.completed", "response.done", "response.incomplete", "content_block_stop", "message_stop":
		case "", "content_block_delta":
			if !strings.Contains(raw, `"response.function_call_arguments.`) && !strings.Contains(raw, `"response.completed"`) &&
				!strings.Contains(raw, `"response.done"`) && !strings.Contains(raw, `"response.incomplete"`) &&
				!strings.Contains(raw, `"content_block_stop"`) && !strings.Contains(raw, `"message_stop"`) && !strings.Contains(raw, `"input_json_delta"`) {
				return []byte(raw), nil
			}
		default:
			return []byte(raw), nil
		}
	}
	payload := make(map[string]any)
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if !json.Valid([]byte(raw)) || dec.Decode(&payload) != nil {
		return []byte(raw), nil
	}
	typeName := jsonutil.FirstString(jsonutil.StringAt(payload, "type"), eventName)
	var changed bool
	var err error
	if renamer.protocol == Responses {
		changed, err = renamer.adaptResponsesFrame(typeName, payload)
	} else {
		changed, err = renamer.adaptAnthropicFrame(typeName, payload)
	}
	if err != nil {
		return nil, err
	}
	// The generic rewrite also handles complete snapshots. Do not run it on
	// an item that has already had its argument fragments converted here.
	if typeName != "response.output_item.added" {
		changed = renameToolInMap(payload, renamer.protocol, renamer.renames) || changed
	}
	if !changed {
		return []byte(raw), nil
	}
	return json.Marshal(payload)
}

func (renamer *streamToolRenamer) adaptResponsesFrame(typ string, event map[string]any) (bool, error) {
	item := jsonutil.MapAt(event, "item")
	key := responseToolKey(event, item)
	switch typ {
	case "response.output_item.added":
		if jsonutil.StringAt(item, "type") != "function_call" && jsonutil.StringAt(item, "type") != "custom_tool_call" {
			return false, nil
		}
		wireName := jsonutil.StringAt(item, "name")
		client, renamed := renamer.renames.Resolve(wireName)
		if renamed {
			item["name"] = client
		}
		if state := renamer.argumentState(key, wireName); state != nil && item["arguments"] != nil {
			adapted, err := renamer.adaptFragment(state, item, "arguments")
			return renamed || adapted, err
		}
		return renamed, nil
	case "response.function_call_arguments.delta":
		if state := renamer.arguments[key]; state != nil {
			return renamer.adaptFragment(state, event, "delta")
		}
	case "response.function_call_arguments.done":
		if state := renamer.arguments[key]; state != nil {
			if state.text.Len() > 0 || !validArgumentObject(event["arguments"]) {
				return false, errStreamToolArguments
			}
			converted, changed := renamer.renames.ToClientArguments(state.wireName, event["arguments"])
			if changed {
				event["arguments"] = converted
			}
			return changed, nil
		}
	case "response.output_item.done":
		if state := renamer.arguments[key]; state != nil {
			if state.text.Len() > 0 || !validArgumentObject(item["arguments"]) {
				return false, errStreamToolArguments
			}
			delete(renamer.arguments, key)
		}
	case "response.completed", "response.done", "response.incomplete":
		if renamer.argumentBytes > 0 {
			return false, errStreamToolArguments
		}
	}
	return false, nil
}

func validArgumentObject(arguments any) bool {
	text, ok := arguments.(string)
	return ok && strings.HasPrefix(strings.TrimSpace(text), "{") && json.Valid([]byte(text))
}

func (renamer *streamToolRenamer) adaptAnthropicFrame(typ string, event map[string]any) (bool, error) {
	key := fmt.Sprint(event["index"])
	switch typ {
	case "content_block_start":
		block := jsonutil.MapAt(event, "content_block")
		if jsonutil.StringAt(block, "type") == "tool_use" {
			renamer.argumentState(key, jsonutil.StringAt(block, "name"))
		}
	case "content_block_delta":
		delta := jsonutil.MapAt(event, "delta")
		if state := renamer.arguments[key]; state != nil && jsonutil.StringAt(delta, "type") == "input_json_delta" {
			return renamer.adaptFragment(state, delta, "partial_json")
		}
	case "content_block_stop":
		if state := renamer.arguments[key]; state != nil && state.text.Len() > 0 {
			return false, errStreamToolArguments
		}
		delete(renamer.arguments, key)
	case "message_stop":
		if renamer.argumentBytes > 0 {
			return false, errStreamToolArguments
		}
	}
	return false, nil
}
