package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"opencode2api/internal/jsonutil"
)

// errStreamUpstreamFailure marks a stream that failed in a way already
// reported, or, in CollapseStream, already recorded, so the caller must not
// report it a second time.
var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

// errStreamNormalTermination reports that the stream reached its own terminal
// event, so the caller treats it as a success.
var errStreamNormalTermination = errors.New("upstream stream terminated normally")

// errSSEUnexpectedEOF reports a stream that ended without a terminal event.
var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

const maxSSEFrameBytes = 16 << 20

var errSSEFrameTooLarge = errors.New("upstream SSE event exceeds 16 MiB")

// streamTermination records how a transcoded stream ended, so the code after
// readSSE can tell a normal terminal event from an upstream error.
type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

// bridgeStreamEvent is one protocol-neutral streaming event, parsed from an
// upstream stream and encoded for the downstream protocol. Kind selects which
// fields matter: "start" (ResponseID, Model), "text", "reasoning" (Text and/or
// Encrypted), "reasoning_signature" (Signature), "tool_start" and "tool_delta"
// (ToolKey, ToolID, ToolName, Text), "usage", "finish" (Stop), "error"
// (Error, ErrorType) and "done".
type bridgeStreamEvent struct {
	Kind       string
	ResponseID string
	Model      string
	Text       string
	Signature  string
	ToolKey    string
	ToolID     string
	ToolName   string
	Stop       string
	Error      string
	ErrorType  string
	Encrypted  string
	Usage      *Usage
}

// TranscodeStream parses an upstream stream of one protocol and re-encodes it
// for another, writing each downstream event as it arrives. It returns the
// usage the emitter collected and whether the upstream reported any.
//
// A nil error means the stream reached its terminal event. Otherwise the error
// is errStreamUpstreamFailure when the failure was already reported in-band,
// the error that ended the read, or ctx.Err() when the client was canceled
// after the stream itself ended cleanly. A stream that ends without a terminal
// event is reported in-band with an error event first, using
// errSSEUnexpectedEOF when nothing more specific is known.
//
// A canceled client gets no synthetic upstream error event: the gateway
// cancels the request context when the client goes away, and writing after
// that only wastes the remaining attempt.
func TranscodeStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		protocol:          from,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	emitter := bridgeStreamEmitter(w, flusher, to, model)
	termination := streamOpen
	readErr := readSSE(reader, func(eventName, data string) error {
		events, err := parser.Parse(eventName, data)
		if err != nil {
			termination = streamErrorTermination
			if emitErr := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: "upstream SSE event could not be decoded", ErrorType: "upstream_error"}); emitErr != nil {
				return emitErr
			}
			return errStreamUpstreamFailure
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				termination = streamNormalTermination
			case "error":
				termination = streamErrorTermination
				event.Error = "upstream stream failed"
				event.ErrorType = "upstream_error"
			}
			if err := emitter.Emit(event); err != nil {
				return err
			}
			if event.Kind == "done" {
				return errStreamNormalTermination
			}
		}
		return nil
	})
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) {
			return emitter.usage, emitter.usageReported, nil
		}
		if ClientCanceled(ctx, readErr) {
			return emitter.usage, emitter.usageReported, readErr
		}
		if termination == streamOpen {
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, emitErr
			}
		}
		return emitter.usage, emitter.usageReported, readErr
	}
	if termination == streamNormalTermination {
		return emitter.usage, emitter.usageReported, nil
	}
	if termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, errStreamUpstreamFailure
	}
	if ClientCanceled(ctx, nil) {
		return emitter.usage, emitter.usageReported, ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, err
	}
	return emitter.usage, emitter.usageReported, errSSEUnexpectedEOF
}

// emitUnexpectedStreamError reports a stream that ended without a terminal
// event, as an in-band error event in the downstream protocol. It returns nil
// once that event is written, and the write error otherwise.
func emitUnexpectedStreamError(emitter *bridgeStreamEncoder, cause error) error {
	message := "upstream SSE stream ended before a terminal event"
	if cause != nil && !errors.Is(cause, errSSEUnexpectedEOF) {
		message = "upstream SSE stream failed"
	}
	err := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: message, ErrorType: "upstream_error"})
	if errors.Is(err, errStreamUpstreamFailure) {
		return nil
	}
	return err
}

// ClientCanceled reports whether a stream error is the client going away,
// either through the stream error itself or through the request context.
func ClientCanceled(ctx context.Context, streamErr error) bool {
	if errors.Is(streamErr, context.Canceled) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// ForwardStream relays complete, valid same-protocol SSE frames unchanged and
// flushes each one while tracking usage. Error frames are replaced with a
// generic failure because upstream errors may echo credentials. A malformed,
// oversized or unterminated frame is never written downstream. Before the
// first frame a failure is a JSON 502; later failures are SSE error events.
// A canceled client gets no synthetic error event.
func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := streamUsageObserver(protocol)
	committed := false
	observer.forward = func(frame []byte) error {
		committed = true
		written, err := w.Write(frame)
		if err == nil && written != len(frame) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return &ClientWriteError{Err: err}
		}
		flusher.Flush()
		return nil
	}
	_, copyErr := io.Copy(observer, reader)
	usage := observer.Finish()
	if errors.Is(copyErr, errStreamNormalTermination) {
		return usage, observer.Reported(), nil
	}
	if ClientCanceled(ctx, copyErr) {
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		return usage, observer.Reported(), copyErr
	}
	var writeErr *ClientWriteError
	if errors.As(copyErr, &writeErr) {
		return usage, observer.Reported(), copyErr
	}
	cause := observer.ParseError()
	if copyErr != nil {
		cause = copyErr
	}
	if cause == nil {
		cause = errSSEUnexpectedEOF
	}
	if !committed {
		w.Header().Del("X-Accel-Buffering")
		WriteError(w, protocol, http.StatusBadGateway, "upstream SSE stream failed", "upstream_error", "")
		return usage, observer.Reported(), cause
	}
	emitter := bridgeStreamEmitter(w, flusher, protocol, model)
	if err := emitUnexpectedStreamError(emitter, cause); err != nil {
		return usage, observer.Reported(), err
	}
	return usage, observer.Reported(), cause
}

// RenameStreamReader wraps an SSE stream reader and rewrites aliased tool call
// names back to the names the client declared before downstream parsers or
// writers consume the stream. An empty rename set returns reader unchanged.
//
// It is a pull-based transform instead of a goroutine plus io.Pipe so a client
// that disconnects mid-stream cannot strand a blocked writer. Fragmented
// argument objects are buffered only for tools with a registered adapter.
func RenameStreamReader(reader io.Reader, protocol Protocol, renames *ToolRenames) io.Reader {
	if renames.Empty() && !renames.Enforcing() {
		return reader
	}
	return &renameStreamReader{
		buffer:  bufio.NewReaderSize(reader, 64<<10),
		renamer: &streamToolRenamer{protocol: protocol, renames: renames, tools: map[string]*streamToolRenameState{}},
	}
}

// renameStreamReader is the io.Reader RenameStreamReader returns: it reads the
// upstream stream line by line, rewrites the data lines that need it, and
// serves the rewritten bytes, carrying an error over to the next Read.
type renameStreamReader struct {
	buffer    *bufio.Reader
	renamer   *streamToolRenamer
	pending   []byte
	err       error
	eventName string
}

// Read returns the rewritten stream bytes, buffering one line at a time.
func (reader *renameStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		var lineBytes []byte
		var err error
		for {
			var part []byte
			part, err = reader.buffer.ReadSlice('\n')
			if len(lineBytes)+len(part) > maxSSEFrameBytes {
				reader.err = errSSEFrameTooLarge
				return 0, reader.err
			}
			lineBytes = append(lineBytes, part...)
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		line := string(lineBytes)
		if len(line) > 0 {
			pending, rewriteErr := reader.rewriteLine(line)
			if rewriteErr != nil {
				reader.err = rewriteErr
				return 0, rewriteErr
			}
			reader.pending = pending
		}
		if err != nil {
			// The final chunk is drained before the error is reported so a
			// truncated stream still delivers every byte the upstream sent.
			reader.err = err
			if errors.Is(err, io.EOF) {
				if reader.renamer.argumentBytes > 0 {
					reader.err = errStreamToolArguments
				} else if final := reader.renamer.finish(); len(final) > 0 {
					if len(reader.pending) > 0 && !bytes.HasSuffix(reader.pending, []byte("\n\n")) && !bytes.HasSuffix(reader.pending, []byte("\r\n\r\n")) {
						if !bytes.HasSuffix(reader.pending, []byte("\n")) {
							reader.pending = append(reader.pending, '\n')
						}
						reader.pending = append(reader.pending, '\n')
					}
					reader.pending = append(reader.pending, final...)
				}
			}
			if len(reader.pending) == 0 {
				return 0, reader.err
			}
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// rewriteLine transforms one complete SSE line, preserving its original bytes
// and terminator whenever nothing needs to be rewritten.
func (reader *renameStreamReader) rewriteLine(line string) ([]byte, error) {
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
		if body == "" {
			reader.eventName = ""
		} else if name, ok := strings.CutPrefix(body, "event:"); ok {
			reader.eventName = strings.TrimSpace(name)
		}
		return []byte(line), nil
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	if raw == "" {
		return []byte(line), nil
	}
	if raw == "[DONE]" {
		if reader.renamer.argumentBytes > 0 {
			return nil, errStreamToolArguments
		}
		if final := reader.renamer.finish(); len(final) > 0 {
			return append(final, []byte(line)...), nil
		}
		return []byte(line), nil
	}
	rewritten, err := reader.renamer.rewriteFrame(reader.eventName, raw)
	if err != nil {
		return nil, err
	}
	if rewritten == nil {
		// The frame belonged to a tool call the client cannot execute; it is
		// dropped whole so the client never sees an unknown tool name.
		return nil, nil
	}
	if string(rewritten) == raw {
		return []byte(line), nil
	}
	if reader.renamer.renames.HasAdapters() && len("data: ")+len(rewritten)+len(terminator) >= maxSSEFrameBytes {
		return nil, errSSEFrameTooLarge
	}
	return []byte("data: " + string(rewritten) + terminator), nil
}

// streamToolRenameState tracks how one tool call's name was resolved while a
// stream is rewritten. Chat deltas may repeat a growing name prefix or append
// fragments, so a value that is still a prefix of a known wire name is held
// back until it either completes the match or diverges.
type streamToolRenameState struct {
	decided          bool
	renamed          bool
	dropped          bool
	pending          string
	pendingArguments strings.Builder
	adapted          *streamArgumentBuffer
	legacy           bool
	callIndex        any
	choiceIndex      any
}

// streamToolRenamer rewrites the tool names of one protocol's SSE frames,
// keeping per-tool state across frames so a name split over several Chat
// chunks is reassembled before it is matched.
type streamToolRenamer struct {
	protocol      Protocol
	renames       *ToolRenames
	tools         map[string]*streamToolRenameState
	order         []string
	arguments     map[string]*streamArgumentBuffer
	argumentBytes int
	// dropFrame asks the reader to emit no bytes for the frame just rewritten.
	dropFrame bool
	// droppedItems and keptItems track Responses items and Anthropic content
	// blocks by their stream key, so argument events that carry no name follow
	// the keep/drop decision made when the item opened.
	droppedItems map[string]bool
	keptItems    map[string]bool
	// keptChoices and droppedChoices decide whether a Chat choice that only
	// produced dropped calls still finishes as a tool call.
	keptChoices    map[string]bool
	droppedChoices map[string]bool
}

// rewriteFrame rewrites one SSE data payload, which is raw JSON text. Frames
// that cannot carry a tool name are returned untouched, so only a payload that
// mentions a client alias (Anthropic and Responses) or "tool_calls" (Chat) is
// decoded.
func (renamer *streamToolRenamer) rewriteFrame(eventName, raw string) ([]byte, error) {
	renamer.dropFrame = false
	if renamer.protocol != Chat {
		if renamer.renames.HasAdapters() || renamer.renames.Enforcing() {
			return renamer.rewriteArgumentFrame(eventName, raw)
		}
		// Anthropic and Responses streams always carry a complete tool name in
		// a single event, so the stateless object rewrite is exact.
		if !mayCarryToolName([]byte(raw)) {
			return []byte(raw), nil
		}
		return RenameResponseTool([]byte(raw), renamer.protocol, renamer.renames), nil
	}
	if !strings.Contains(raw, `"tool_calls"`) && !strings.Contains(raw, `"function_call"`) && !strings.Contains(raw, `"finish_reason"`) {
		return []byte(raw), nil
	}
	return renamer.rewriteChatFrame(raw)
}

// rewriteChatFrame rewrites the tool names of one Chat chunk. Tool calls are
// keyed by index, then id, then array position, and each name fragment is fed
// through applyNameFragment so a name split across chunks is restored as a
// whole. It returns the frame unchanged when nothing was rewritten.
func (renamer *streamToolRenamer) rewriteChatFrame(raw string) ([]byte, error) {
	var payload map[string]any
	if renamer.renames.HasAdapters() {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if !json.Valid([]byte(raw)) || decoder.Decode(&payload) != nil {
			return []byte(raw), nil
		}
	} else if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw), nil
	}
	changed := false
	for choicePosition, rawChoice := range jsonutil.SliceAt(payload, "choices") {
		choice, _ := rawChoice.(map[string]any)
		choiceIndex := choice["index"]
		if choiceIndex == nil {
			choiceIndex = choicePosition
		}
		finishReason, hasFinishReason := choice["finish_reason"].(string)
		finished := hasFinishReason && finishReason != ""
		seen := map[string]bool{}
		choiceKey := fmt.Sprint(choiceIndex)
		for _, container := range []string{"delta", "message"} {
			message, _ := choice[container].(map[string]any)
			if message == nil {
				continue
			}
			if calls := jsonutil.SliceAt(message, "tool_calls"); len(calls) > 0 {
				kept := make([]any, 0, len(calls))
				for callIndex, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					function, _ := call["function"].(map[string]any)
					if function == nil {
						kept = append(kept, rawCall)
						continue
					}
					// Tool indices restart at zero in each choice. Keeping the choice
					// in the key prevents one completion from consuming another's name.
					key := fmt.Sprint(choiceIndex) + "\x00" + fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id"), callIndex))
					state := renamer.toolState(key)
					state.callIndex = jsonutil.FirstAny(call["index"], callIndex)
					state.choiceIndex = choiceIndex
					seen[key] = true
					rewritten, err := renamer.rewriteChatTool(state, function, finished)
					if err != nil {
						return nil, err
					}
					if state.dropped {
						renamer.noteDroppedChoice(choiceKey)
						changed = true
						continue
					}
					if state.decided {
						// A call whose name is still held back is not counted
						// as kept until it resolves.
						renamer.noteKeptChoice(choiceKey)
					}
					kept = append(kept, rawCall)
					changed = rewritten || changed
				}
				if len(kept) == 0 {
					delete(message, "tool_calls")
				} else {
					message["tool_calls"] = kept
				}
			}
			if function := jsonutil.MapAt(message, "function_call"); function != nil {
				key := fmt.Sprint(choiceIndex) + "\x01legacy"
				state := renamer.toolState(key)
				state.legacy = true
				state.choiceIndex = choiceIndex
				seen[key] = true
				rewritten, err := renamer.rewriteChatTool(state, function, finished)
				if err != nil {
					return nil, err
				}
				if state.dropped {
					delete(message, "function_call")
					renamer.noteDroppedChoice(choiceKey)
					changed = true
					continue
				}
				if state.decided {
					renamer.noteKeptChoice(choiceKey)
				}
				changed = rewritten || changed
			}
		}
		if finished {
			if renamer.argumentBytes > 0 {
				for _, state := range renamer.tools {
					if state.adapted != nil && state.adapted.text.Len() > 0 && fmt.Sprint(state.choiceIndex) == fmt.Sprint(choiceIndex) {
						return nil, errStreamToolArguments
					}
				}
			}
			for _, key := range renamer.order {
				state := renamer.tools[key]
				if seen[key] || !state.hasPendingCall() || fmt.Sprint(state.choiceIndex) != fmt.Sprint(choiceIndex) {
					continue
				}
				delta, _ := choice["delta"].(map[string]any)
				if delta == nil {
					delta = map[string]any{}
					choice["delta"] = delta
				}
				renamer.appendPendingCall(state, delta)
				if state.dropped {
					renamer.noteDroppedChoice(choiceKey)
				} else {
					renamer.noteKeptChoice(choiceKey)
				}
				changed = true
			}
			// A choice whose only calls were dropped must not finish as a tool
			// call: the client would wait for a call it never received.
			if renamer.droppedChoices[choiceKey] && !renamer.keptChoices[choiceKey] {
				if finishReason == "tool_calls" || finishReason == "function_call" {
					choice["finish_reason"] = "stop"
					changed = true
				}
			}
		}
	}
	if !changed {
		return []byte(raw), nil
	}
	return json.Marshal(payload)
}

func (renamer *streamToolRenamer) rewriteChatTool(state *streamToolRenameState, function map[string]any, finished bool) (bool, error) {
	changed := false
	if name, _ := function["name"].(string); name != "" {
		changed = renamer.applyNameFragment(state, function, name)
	}
	if arguments, exists := function["arguments"]; exists && !state.decided {
		if renamer.renames.HasLongerPrefix(state.pending) {
			if fragment, ok := arguments.(string); ok {
				if len(fragment) > maxSSEFrameBytes-state.pendingArguments.Len() {
					return false, errSSEFrameTooLarge
				}
				state.pendingArguments.WriteString(fragment)
				delete(function, "arguments")
				changed = true
			}
		} else {
			if state.pending != "" {
				function["name"] = state.pending
				changed = true
			}
			state.decided = true
		}
	}
	if state.decided {
		changed = renamer.releasePendingArguments(state, function) || changed
		adapted, err := renamer.adaptChatArguments(state, function)
		return adapted || changed, err
	}
	if finished && state.hasPendingCall() {
		changed = renamer.finishState(state, function) || changed
	}
	return changed, nil
}

// releasePendingArguments prepends the argument fragments held back while the
// tool name was still ambiguous to the current frame's arguments.
func (renamer *streamToolRenamer) releasePendingArguments(state *streamToolRenameState, function map[string]any) bool {
	if state.pendingArguments.Len() == 0 {
		return false
	}
	arguments := state.pendingArguments.String()
	if current, exists := function["arguments"]; exists {
		fragment, ok := current.(string)
		if !ok {
			return false
		}
		arguments += fragment
	}
	function["arguments"] = arguments
	state.pendingArguments.Reset()
	return true
}

// finishState settles a call the choice finished while its name was still
// ambiguous: the held-back name and arguments are emitted verbatim.
func (renamer *streamToolRenamer) finishState(state *streamToolRenameState, function map[string]any) bool {
	changed := false
	if state.pending != "" && !renamer.renames.Keep(state.pending) {
		// The held-back name never resolved to a declared client tool, so the
		// call cannot run; drop it instead of emitting a name the client would
		// reject.
		state.dropped, state.decided, state.renamed = true, true, false
		state.pending = ""
		state.pendingArguments.Reset()
		return true
	}
	if state.pending != "" {
		function["name"] = state.pending
		changed = true
	}
	if renamer.releasePendingArguments(state, function) {
		changed = true
	}
	state.decided, state.renamed = true, false
	return changed
}

func (state *streamToolRenameState) hasPendingCall() bool {
	return !state.decided && (state.pending != "" || state.pendingArguments.Len() > 0)
}

// appendPendingCall adds a call held back during name resolution to the choice delta.
func (renamer *streamToolRenamer) appendPendingCall(state *streamToolRenameState, delta map[string]any) {
	function := map[string]any{}
	renamer.finishState(state, function)
	if state.dropped {
		return
	}
	if state.legacy {
		delta["function_call"] = function
		return
	}
	call := map[string]any{"index": state.callIndex, "function": function}
	delta["tool_calls"] = append(jsonutil.SliceAt(delta, "tool_calls"), call)
}

// finish flushes every call still held back when the stream ends without a
// finish_reason, as one extra Chat frame. It returns nil when nothing is held.
func (renamer *streamToolRenamer) finish() []byte {
	if renamer.protocol != Chat {
		return nil
	}
	type choiceCalls struct {
		index any
		delta map[string]any
	}
	var groups []*choiceCalls
	byIndex := map[string]*choiceCalls{}
	for _, key := range renamer.order {
		state := renamer.tools[key]
		if !state.hasPendingCall() {
			continue
		}
		index := state.choiceIndex
		if index == nil {
			index = 0
		}
		indexKey := fmt.Sprint(index)
		group := byIndex[indexKey]
		if group == nil {
			group = &choiceCalls{index: index, delta: map[string]any{}}
			byIndex[indexKey] = group
			groups = append(groups, group)
		}
		renamer.appendPendingCall(state, group.delta)
	}
	if len(groups) == 0 {
		return nil
	}
	choices := make([]any, 0, len(groups))
	for _, group := range groups {
		choices = append(choices, map[string]any{
			"index":         group.index,
			"delta":         group.delta,
			"finish_reason": nil,
		})
	}
	encoded, err := json.Marshal(map[string]any{"choices": choices})
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), encoded...), '\n', '\n')
}

// applyNameFragment folds one name fragment into a tool's rename state and
// reports whether the frame changed. Once the accumulated name matches a wire
// name exactly, it is replaced by the client name; while it is still a proper
// prefix of one the name is held back; on divergence the accumulated name is
// written out as is. After a resolved rename, any further fragment is dropped,
// because it belongs to a longer name the client never declared.
func (renamer *streamToolRenamer) applyNameFragment(state *streamToolRenameState, function map[string]any, fragment string) bool {
	if state.decided {
		if state.dropped || !state.renamed {
			return false
		}
		// A resolved alias accepts no further fragments: any continuation
		// belongs to a longer name the client never declared.
		if _, exists := function["name"]; exists {
			delete(function, "name")
			return true
		}
		return false
	}
	state.pending = mergeToolName(state.pending, fragment)
	if client, ok := renamer.renames.Client(state.pending); ok {
		function["name"] = client
		state.decided, state.renamed = true, true
		return true
	}
	if renamer.renames.HasLongerPrefix(state.pending) {
		// Still a prefix of a known wire name: hold the fragment until a later
		// frame either completes the match or diverges.
		delete(function, "name")
		return true
	}
	// The name is complete. Resolve folds a spelling that differs from exactly
	// one declared client tool only in case or separators; a name nothing
	// declared is written out as the model sent it.
	client, renamed := renamer.renames.Resolve(state.pending)
	if !renamer.renames.Keep(client) {
		state.dropped, state.decided, state.renamed = true, true, false
		delete(function, "name")
		return true
	}
	function["name"] = client
	state.decided, state.renamed = true, renamed
	return true
}

// noteKeptChoice records that a choice produced at least one call the client
// can execute, so its tool-call finish reason stays truthful.
func (renamer *streamToolRenamer) noteKeptChoice(key string) {
	if renamer.keptChoices == nil {
		renamer.keptChoices = map[string]bool{}
	}
	renamer.keptChoices[key] = true
}

// noteDroppedChoice records that a choice lost at least one call.
func (renamer *streamToolRenamer) noteDroppedChoice(key string) {
	if renamer.droppedChoices == nil {
		renamer.droppedChoices = map[string]bool{}
	}
	renamer.droppedChoices[key] = true
}

// toolState returns the rename state for a tool key, creating it on first
// sight.
func (renamer *streamToolRenamer) toolState(key string) *streamToolRenameState {
	if state := renamer.tools[key]; state != nil {
		return state
	}
	state := &streamToolRenameState{}
	renamer.tools[key] = state
	renamer.order = append(renamer.order, key)
	return state
}

// streamUsageTracker is an io.Writer that splits a relayed stream into SSE
// frames and parses them for usage, while recording whether the stream reached
// a terminal or error event and whether any frame failed to parse. It never
// modifies the bytes it is given.
type streamUsageTracker struct {
	parser   *bridgeStreamParser
	forward  func([]byte) error
	buffer   []byte
	usage    Usage
	reported bool
	normal   bool
	error    bool
	parseErr error
}

// streamUsageObserver builds a usage tracker whose parser reads the given
// protocol.
func streamUsageObserver(protocol Protocol) *streamUsageTracker {
	return &streamUsageTracker{parser: &bridgeStreamParser{
		protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

// Write validates each complete frame before passing its original bytes to
// forward. The bounded tail is retained until a delimiter arrives; errors and
// the terminal event stop copying before any later bytes can be forwarded.
func (observer *streamUsageTracker) Write(data []byte) (int, error) {
	if observer.parseErr != nil {
		return 0, observer.parseErr
	}
	remaining := data
	for len(remaining) > 0 {
		count := min(len(remaining), maxSSEFrameBytes+1-len(observer.buffer))
		observer.buffer = append(observer.buffer, remaining[:count]...)
		remaining = remaining[count:]
		for {
			index, width := nextSSEBoundary(observer.buffer)
			if index < 0 {
				break
			}
			if index+width > maxSSEFrameBytes {
				observer.parseErr = errSSEFrameTooLarge
				break
			}
			observer.consume(observer.buffer[:index+width])
			if observer.parseErr != nil {
				return len(data) - len(remaining), observer.parseErr
			}
			if observer.error {
				return len(data) - len(remaining), errStreamUpstreamFailure
			}
			if observer.forward != nil {
				if err := observer.forward(observer.buffer[:index+width]); err != nil {
					return len(data) - len(remaining), err
				}
			}
			observer.buffer = observer.buffer[index+width:]
			if observer.normal {
				return len(data) - len(remaining), errStreamNormalTermination
			}
		}
		if len(observer.buffer) > maxSSEFrameBytes {
			observer.parseErr = errSSEFrameTooLarge
		}
		if observer.parseErr != nil {
			observer.buffer = nil
			return len(data) - len(remaining), observer.parseErr
		}
	}
	return len(data), nil
}

// Finish returns the merged usage and discards the buffered tail.
func (observer *streamUsageTracker) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	return observer.usage
}

func (observer *streamUsageTracker) Reported() bool { return observer.reported }

func (observer *streamUsageTracker) ParseError() error { return observer.parseErr }

// consume parses one complete SSE frame and records its terminal events,
// usage and parse error before Write decides whether the frame can be relayed.
func (observer *streamUsageTracker) consume(frame []byte) {
	err := readSSE(bytes.NewReader(frame), func(eventName, data string) error {
		events, err := observer.parser.Parse(eventName, data)
		if err != nil {
			if observer.parseErr == nil {
				observer.parseErr = err
			}
			return nil
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				observer.normal = true
			case "error":
				observer.error = true
			}
			if event.Usage != nil {
				observer.reported = true
				mergeBridgeUsage(&observer.usage, *event.Usage)
			}
		}
		return nil
	})
	if err != nil && observer.parseErr == nil {
		observer.parseErr = err
	}
}

// nextSSEBoundary finds the end of the first complete SSE frame in data: it
// returns the index where the frame's terminator starts and the terminator's
// width, preferring whichever of a blank line ("\n\n") or a CRLF blank line
// ("\r\n\r\n") comes first. No complete frame yields -1.
func nextSSEBoundary(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 {
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	}
	if crlf >= 0 && crlf < lf {
		return crlf, 4
	}
	return lf, 2
}

// readSSE reads a stream and calls handler once per complete SSE record, with
// the record's event name and its data lines joined by newlines. A record ends
// at a blank line; comment lines are skipped, and a record without any data
// line is not delivered. A final record whose data is not terminated by a
// blank line is an unexpected EOF rather than a complete event. Scanner
// errors, and errors returned by handler, end the read.
func readSSE(reader io.Reader, handler func(eventName, data string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxSSEFrameBytes)
	var eventName string
	var dataLines []string
	frameBytes := 0
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		err := handler(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		frameBytes += len(line) + 1
		if frameBytes > maxSSEFrameBytes {
			return errSSEFrameTooLarge
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			frameBytes = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(dataLines) > 0 {
		// A blank line is the SSE record delimiter. Do not parse a final
		// unterminated record as a complete frame; an EOF without a terminal
		// event is handled by the caller as a truncated stream.
		return errSSEUnexpectedEOF
	}
	return nil
}
