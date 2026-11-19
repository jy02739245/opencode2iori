package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func TestInferenceErrorPreservesStreamPreference(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, stream := range []bool{false, true} {
			body, _ := json.Marshal(map[string]any{"stream": stream})
			meta := &telemetry.RequestMeta{Stream: !stream}
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			r = r.WithContext(telemetry.WithRequestMeta(r.Context(), meta))
			w := httptest.NewRecorder()
			(&Gateway{}).handleInference(protocol)(w, r)
			if w.Code != http.StatusBadRequest || meta.Stream != stream {
				t.Fatalf("%s stream=%v: status=%d meta.Stream=%v", protocol, stream, w.Code, meta.Stream)
			}
		}
	}
}

func TestAuxiliaryRequestIsStreamedWithOfficialTools(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, v1 := range []bool{false, true} {
			for _, explicitEmpty := range []bool{false, true} {
				body := map[string]any{"model": "deepseek-v4.1-flash-free", "stream": false,
					"messages": []any{map[string]any{"role": "system", "content": "You generate a descriptive title for this conversation"}, map[string]any{"role": "user", "content": "Harness integration test"}}}
				if explicitEmpty {
					body["tools"] = []any{}
				}
				if !isTitleGenerationRequest(body) {
					t.Fatal("fixture must exercise the recognized title path")
				}
				encoded, _ := json.Marshal(body)
				prepared, _ := prepareAnonymousBody(encoded, protocol, v1, nil)
				var payload map[string]any
				if err := json.Unmarshal(prepared, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["stream"] != true {
					t.Fatalf("%s v1=%v: auxiliary request is not streaming", protocol, v1)
				}
				tools, _ := payload["tools"].([]any)
				if len(tools) != len(officialToolNames(v1)) {
					t.Fatalf("%s v1=%v empty=%v: got %d tools, want full official set", protocol, v1, explicitEmpty, len(tools))
				}
				for index, name := range officialToolNames(v1) {
					if !reflect.DeepEqual(tools[index], cloneJSONMap(officialTool(protocol, name, v1, ""))) {
						t.Fatalf("%s v1=%v: incorrect official definition for %s", protocol, v1, name)
					}
				}
			}
		}
	}
}

func TestDSHTitleKeepsStreamingWithOfficialTools(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","stream":false,"messages":[{"role":"system","content":"Create a concise title for an AI coding-assistant session from the supplied human messages."},{"role":"user","content":"Generate the session title from this JSON array of human messages:\n[{\"seq\":1,\"text\":\"Harness integration test\"}]"}]}`)
	prepared, _ := prepareAnonymousBody(body, wire.Chat, false, nil)
	var payload map[string]any
	if err := json.Unmarshal(prepared, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true {
		t.Fatal("DSH title request must request upstream streaming")
	}
	if tools, _ := payload["tools"].([]any); len(tools) != len(officialToolNames(false)) {
		t.Fatal("DSH title request must carry the complete official tool set")
	}
	if !strings.Contains(string(prepared), "Create a concise title for an AI coding-assistant session") {
		t.Fatal("DSH title instructions were dropped")
	}
}
