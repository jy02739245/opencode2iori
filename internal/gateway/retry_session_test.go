package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

type proxyRetryCapture struct {
	proxy  int
	header http.Header
	body   string
	remote string
	close  bool
}

func TestProxyFailoverSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keyed, pool bool
		v1          bool
		proxies     int
		statuses    []int
		renew       []bool
		maxExits    int
		requests    int
	}{
		{name: "anonymous 429", proxies: 2, statuses: []int{429, 200}, renew: []bool{false, true}},
		{name: "anonymous 500 preserves session", proxies: 2, statuses: []int{500, 200}, renew: []bool{false, false}},
		{name: "each proxy 429 renews", proxies: 3, statuses: []int{429, 429, 200}, renew: []bool{false, true, true}},
		{name: "override survives later 500", proxies: 3, statuses: []int{429, 500, 200}, renew: []bool{false, true, false}},
		{name: "static exit limit unchanged", proxies: 2, maxExits: 1, statuses: []int{429}, renew: []bool{false}},
		{name: "pool new connection", proxies: 1, pool: true, statuses: []int{429, 200}, renew: []bool{false, true}},
		{name: "pool attempt limit unchanged", proxies: 1, pool: true, statuses: []int{429, 429, 429}, renew: []bool{false, true, true}},
		{name: "key changes proxy", keyed: true, proxies: 2, statuses: []int{429, 200}, renew: []bool{false, true}},
		{name: "key changes on same proxy", keyed: true, proxies: 1, statuses: []int{429, 200}, renew: []bool{false, false}},
		{name: "v1 existing headers only", keyed: true, v1: true, proxies: 2, statuses: []int{429, 200}, renew: []bool{false, true}},
		{name: "next client request starts clean", proxies: 2, requests: 2, statuses: []int{429, 200, 200}, renew: []bool{false, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captures := make(chan proxyRetryCapture, 32)
			var count atomic.Int32
			cfg := config.Config{
				Anonymous:   !tc.keyed,
				Retry:       config.RetryConfig{MaxAttempts: 3, TimeoutSeconds: 5},
				Performance: config.PerformanceConfig{ConnectTimeoutSeconds: 1, FailureCooldownSeconds: 1, MaxAnonymousExits: tc.maxExits},
				Upstream:    config.UpstreamConfig{Anonymous: "http://upstream.invalid/inference", Zen: "http://upstream.invalid/inference"},
			}
			if tc.v1 {
				cfg.Upstream.Zen = "http://upstream.invalid/zen"
			}
			for index := range tc.proxies {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read proxy request: %v", err)
					}
					position := int(count.Add(1)) - 1
					captures <- proxyRetryCapture{proxy: index, header: r.Header.Clone(), body: string(body), remote: r.RemoteAddr, close: r.Close}
					status := http.StatusInternalServerError
					if position < len(tc.statuses) {
						status = tc.statuses[position]
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, `{"fixture":true}`)
				}))
				t.Cleanup(server.Close)
				entry := server.URL
				if tc.pool {
					entry += "#pool"
				}
				cfg.Proxies = append(cfg.Proxies, entry)
			}
			if tc.keyed {
				cfg.ZenKeys = []string{"fixture-key-a", "fixture-key-b"}
				cfg.ProxyBindings = map[string]string{}
				for index, key := range cfg.ZenKeys {
					cfg.ProxyBindings[config.Fingerprint(key)] = cfg.Proxies[index%tc.proxies]
				}
			}
			var logs bytes.Buffer
			g, err := New(cfg, slog.New(slog.NewJSONHandler(&logs, nil)), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, proxy := range g.transports.items {
					proxy.client.CloseIdleConnections()
					proxy.bun.CloseIdleConnections()
				}
			})
			ids := identity.RequestIDs{
				Session: identity.CanonicalSessionID("proxy-fixture-root"), ChildSession: identity.CanonicalSessionID("proxy-fixture-child"),
				ParentSession: identity.CanonicalSessionID("proxy-fixture-parent"), Project: "fixed-project", Request: "req-fixture", Message: "msg-fixture",
			}
			original := ids
			meta := &telemetry.RequestMeta{Request: ids.Request, Session: ids.Session}
			ctx, cancel := context.WithTimeout(telemetry.WithRequestMeta(context.Background(), meta), 5*time.Second)
			defer cancel()
			route := models.Route{ID: "fixture-model", Tier: config.TierZen, Protocol: wire.Chat, Anonymous: !tc.keyed}
			bodies := map[config.Tier][]byte{config.TierZen: []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"Reply with OK"}],"stream":true}`)}
			for range max(tc.requests, 1) {
				resp, _, err := g.doUpstream(ctx, route, bodies, ids)
				if err != nil || resp == nil {
					t.Fatalf("upstream response: %v, %v", resp, err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != tc.statuses[len(tc.statuses)-1] {
					t.Fatalf("final status = %d", resp.StatusCode)
				}
			}
			if int(count.Load()) != len(tc.statuses) {
				t.Fatalf("attempts = %d, want %d", count.Load(), len(tc.statuses))
			}
			var first proxyRetryCapture
			var previous proxyRetryCapture
			var active string
			for index := range tc.statuses {
				capture := <-captures
				if index == 0 {
					first = capture
				}
				if capture.body != first.body || capture.header.Get("x-opencode-project") != projectID(ids.Project) {
					t.Fatal("failover changed the body or project")
				}
				if tc.renew[index] {
					fresh := capture.header.Get("x-opencode-session")
					if fresh == previous.header.Get("x-opencode-session") || fresh == ids.Session || fresh == ids.ChildSession || fresh == ids.ParentSession || identity.CanonicalSessionID(fresh) != fresh {
						t.Fatalf("invalid or reused retry session: %q", fresh)
					}
					active = fresh
					if !tc.pool && capture.proxy == previous.proxy {
						t.Fatal("session renewed without changing static proxy")
					}
				}
				if tc.requests > 1 && index == 2 {
					active = ""
				}
				for _, header := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
					want := first.header.Get(header)
					if active != "" && want != "" {
						want = active
					}
					if capture.header.Get(header) != want {
						t.Fatalf("attempt %d %s = %q, want %q", index+1, header, capture.header.Get(header), want)
					}
				}
				for _, header := range []string{"x-opencode-parent-session-id", "x-parent-session-id"} {
					want := first.header.Get(header)
					if active != "" {
						want = ""
					}
					if capture.header.Get(header) != want {
						t.Fatalf("attempt %d retained old parent affinity", index+1)
					}
				}
				if tc.pool && (!capture.close || index > 0 && capture.remote == previous.remote) {
					t.Fatal("pool attempt did not use a new connection")
				}
				previous = capture
			}
			if !reflect.DeepEqual(ids, original) || meta.Session != original.Session || len(meta.Legs) != len(tc.statuses) {
				t.Fatal("changed client identity or attempt accounting")
			}
			if strings.Count(logs.String(), `"event":"proxy_session_retry"`) != countTrue(tc.renew) {
				t.Fatalf("unexpected session renewal log: %s", logs.String())
			}
		})
	}
}

func countTrue(values []bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}
