package gateway

import (
	"context"
	"net/http"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/telemetry"
)

type proxySessionRetryKey struct{}

// Attempts are sequential and share this state only within one client request.
type proxySessionRetry struct {
	proxy   *proxyTransport
	status  int
	session string
}

func withProxySessionRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, proxySessionRetryKey{}, &proxySessionRetry{})
}

func (g *Gateway) doProxyInferenceAttempt(proxy *proxyTransport, client *http.Client, req *http.Request, firstEventTimeout time.Duration) (*http.Response, error) {
	retry, _ := req.Context().Value(proxySessionRetryKey{}).(*proxySessionRetry)
	if retry != nil {
		if retry.status == http.StatusTooManyRequests && retry.proxy != nil && proxy != nil && (proxy != retry.proxy || proxy.pool) {
			previous := retry.session
			if previous == "" {
				previous = req.Header.Get("x-opencode-session")
			}
			retry.session = identity.CanonicalSessionID(identity.RandomID("proxy-retry", 16))
			requestID, attempt := "", 0
			if meta := telemetry.MetaFromContext(req.Context()); meta != nil {
				requestID, attempt = meta.Request, meta.Attempts
			}
			g.logger.Info("renewing upstream session after proxy 429", "component", "upstream", "event", "proxy_session_retry", "request_id", requestID, "attempt", attempt, "reason", "http_429", "previous_session", previous, "session", retry.session, "previous_proxy", config.RedactURL(retry.proxy.name), "proxy", config.RedactURL(proxy.name))
		}
		if retry.session != "" {
			for _, header := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
				if req.Header.Get(header) != "" {
					req.Header.Set(header, retry.session)
				}
			}
			// Old parent affinity would otherwise keep the retry on its original session.
			req.Header.Del("x-opencode-parent-session-id")
			req.Header.Del("x-parent-session-id")
		}
	}
	resp, err := doInferenceAttempt(client, req, firstEventTimeout, g.attemptTimeout)
	if retry != nil {
		retry.proxy, retry.status = proxy, 0
		if err == nil && resp != nil {
			retry.status = resp.StatusCode
		}
	}
	return resp, err
}
