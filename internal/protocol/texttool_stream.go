package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
)

// TextToolStreamReader rewrites a same-protocol Chat SSE stream so a tool call
// the upstream model wrote as DSML text becomes a real tool_calls delta. Chat
// streams that carry no such markup pass through unchanged.
//
// It exists because a stream that is forwarded byte for byte (the client and
// the upstream speak the same protocol) never reaches the bridge, so the
// extractor used by TranscodeStream would not see it. Every other protocol
// pair is transcoded and filtered there.
func TextToolStreamReader(reader io.Reader, protocol Protocol) io.Reader {
	if protocol != Chat {
		return reader
	}
	return &textToolStreamReader{
		buffer:  bufio.NewReaderSize(reader, 64<<10),
		created: time.Now().Unix(),
	}
}

// textToolStreamReader serves a Chat stream line by line, splitting DSML
// markup out of content deltas and appending the equivalent tool_calls chunks.
type textToolStreamReader struct {
	buffer    *bufio.Reader
	extractor textToolExtractor
	pending   []byte
	err       error
	// calls counts the tool calls recovered so far; finished records whether a
	// finish_reason frame was already served.
	calls    int
	finished bool
	// id, model and created come from the upstream chunks so synthesized ones
	// look like the rest of the stream.
	id      string
	model   string
	created int64
}

// Read returns the rewritten stream bytes, buffering one line at a time.
func (reader *textToolStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		line, err := reader.buffer.ReadString('\n')
		if len(line) > 0 {
			reader.pending = reader.rewriteLine(line)
		}
		if err != nil {
			reader.err = err
			if errors.Is(err, io.EOF) {
				if final := reader.finish(); len(final) > 0 {
					if len(reader.pending) > 0 && !bytes.HasSuffix(reader.pending, []byte("\n")) {
						reader.pending = append(reader.pending, '\n')
					}
					reader.pending = append(reader.pending, final...)
				}
			}
			if len(reader.pending) == 0 {
				return 0, err
			}
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// rewriteLine transforms one complete SSE line. A line that carries no content
// delta is returned as it arrived.
func (reader *textToolStreamReader) rewriteLine(line string) []byte {
	body, terminator := line, ""
	if trimmed := strings.TrimSuffix(line, "\n"); trimmed != line {
		terminator = "\n"
		body = trimmed
		if trimmed = strings.TrimSuffix(body, "\r"); trimmed != body {
			terminator = "\r\n"
			body = trimmed
		}
	}
	if !strings.HasPrefix(body, "data:") {
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	if raw == "" {
		return []byte(line)
	}
	if raw == "[DONE]" {
		final := reader.finish()
		if len(final) == 0 {
			return []byte(line)
		}
		return append(final, []byte(line)...)
	}
	rewritten, extra := reader.rewriteFrame(raw)
	if len(extra) == 0 && rewritten == raw {
		return []byte(line)
	}
	out := make([]byte, 0, len(rewritten)+len(extra)+16)
	if rewritten != "" {
		out = append(out, "data: "...)
		out = append(out, rewritten...)
		out = append(out, terminator...)
		out = append(out, '\n')
	}
	out = append(out, extra...)
	return out
}

// rewriteFrame rewrites one Chat chunk. It returns the rewritten payload (empty
// when the frame carried nothing else) and any synthesized frames.
func (reader *textToolStreamReader) rewriteFrame(raw string) (string, []byte) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return raw, nil
	}
	if id := jsonutil.StringAt(payload, "id"); id != "" {
		reader.id = id
	}
	if model := jsonutil.StringAt(payload, "model"); model != "" {
		reader.model = model
	}
	if created, ok := payload["created"].(float64); ok && created > 0 {
		reader.created = int64(created)
	}
	choices := jsonutil.SliceAt(payload, "choices")
	if len(choices) == 0 {
		return raw, nil
	}
	var calls []textToolCall
	changed := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			visible, found := reader.extractor.Feed(content)
			calls = append(calls, found...)
			if visible != content {
				changed = true
				if visible == "" {
					delete(delta, "content")
				} else {
					delta["content"] = visible
				}
			}
		}
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			reader.finished = true
			if reader.calls+len(calls) > 0 && (reason == "stop" || reason == "end_turn") {
				choice["finish_reason"] = "tool_calls"
				changed = true
			}
		}
	}
	extra := reader.callFrames(calls)
	if !changed && len(extra) == 0 {
		return raw, nil
	}
	if frameIsEmpty(choices) {
		// The chunk carried nothing but markup, so the client has nothing to
		// render for it; the recovered calls are served in their own frames.
		return "", extra
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return raw, extra
	}
	return string(encoded), extra
}

// frameIsEmpty reports whether every choice carries neither a delta nor a stop
// reason, which is what a chunk reduced to bare markup looks like.
func frameIsEmpty(choices []any) bool {
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok && len(delta) > 0 {
			return false
		}
		if reason, exists := choice["finish_reason"]; exists && reason != nil {
			return false
		}
		for key := range choice {
			switch key {
			case "index", "delta", "finish_reason":
			default:
				return false
			}
		}
	}
	return true
}

// finish flushes the held text and calls when the stream ends. A call recovered
// after the last chunk gets its own finish chunk, so the client sees a tool
// stop rather than a truncated answer.
func (reader *textToolStreamReader) finish() []byte {
	visible, calls := reader.extractor.Flush()
	var out bytes.Buffer
	if visible != "" {
		out.Write(reader.chunkFrame(map[string]any{"content": visible}, nil))
	}
	out.Write(reader.callFrames(calls))
	if (reader.calls > 0 || len(calls) > 0) && !reader.finished {
		out.Write(reader.chunkFrame(map[string]any{}, "tool_calls"))
	}
	return out.Bytes()
}

// callFrames renders recovered calls as tool_calls deltas.
func (reader *textToolStreamReader) callFrames(calls []textToolCall) []byte {
	if len(calls) == 0 {
		return nil
	}
	var out bytes.Buffer
	for _, call := range calls {
		index := reader.calls
		reader.calls++
		out.Write(reader.chunkFrame(map[string]any{"tool_calls": []any{map[string]any{
			"index": index,
			"id":    identity.RandomID("call", 12),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}}}, nil))
	}
	return out.Bytes()
}

// chunkFrame encodes one Chat chunk in the stream's own identity.
func (reader *textToolStreamReader) chunkFrame(delta map[string]any, finish any) []byte {
	id := reader.id
	if id == "" {
		id = identity.RandomID("chatcmpl", 12)
	}
	model := reader.model
	created := reader.created
	if created == 0 {
		created = time.Now().Unix()
	}
	encoded, err := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	})
	if err != nil {
		return nil
	}
	return []byte("data: " + string(encoded) + "\n\n")
}
