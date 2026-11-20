package gateway

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func TestClientEnvironmentRequestIsolation(t *testing.T) {
	gitRepo := true
	fallback := config.PromptEnvironmentConfig{
		WorkingDirectory: "/gateway", WorkspaceRoot: "/gateway-root", Platform: "linux", IsGitRepo: &gitRepo,
	}
	g := &Gateway{cfg: config.Config{Upstream: config.UpstreamConfig{PromptEnvironment: &fallback}}}
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			payload := map[string]any{"client_environment": map[string]any{"working_directory": `C:\client`, "platform": "windows"}}
			r, stripped, err := g.withClientEnvironment(httptest.NewRequest("POST", "/", nil), payload, protocol)
			if err != nil || !stripped {
				t.Fatalf("explicit environment: stripped=%v err=%v", stripped, err)
			}
			if _, exists := payload["client_environment"]; exists {
				t.Fatal("gateway-only extension was not consumed")
			}
			want := config.PromptEnvironmentConfig{WorkingDirectory: `C:\client`, Platform: "win32"}
			if got := g.requestEnvironment(r.Context()); !reflect.DeepEqual(*got, want) {
				t.Fatalf("client snapshot = %#v, want %#v", *got, want)
			}
			next, _, err := g.withClientEnvironment(httptest.NewRequest("POST", "/", nil), map[string]any{}, protocol)
			if err != nil || !reflect.DeepEqual(*g.requestEnvironment(next.Context()), fallback) {
				t.Fatalf("next request did not use its own fallback: %v", err)
			}
			if got := g.requestEnvironment(r.Context()); !reflect.DeepEqual(*got, want) {
				t.Fatal("later request changed the earlier snapshot")
			}
		})
	}
	for _, invalid := range []any{"invalid", map[string]any{"platform": "unsupported"}, map[string]any{"is_git_repo": "yes"}} {
		_, _, err := g.withClientEnvironment(httptest.NewRequest("POST", "/", nil), map[string]any{"client_environment": invalid}, wire.Chat)
		if err == nil {
			t.Fatalf("accepted malformed environment: %#v", invalid)
		}
	}
}

func TestClientEnvironmentUnknownPrompt(t *testing.T) {
	prompt := opencodeEnvironment("fixture-model", nil)
	for _, field := range []string{"Working directory", "Workspace root folder", "Is directory a git repo", "Platform"} {
		if !strings.Contains(prompt, field+": unknown") {
			t.Fatalf("missing unknown value for %s", field)
		}
	}
	if got := v2ShellDescription("Run on Linux using bash", ""); got != "Run in the client's shell environment" {
		t.Fatalf("unknown shell description = %q", got)
	}
}
