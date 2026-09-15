package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

const relayPushBody = `{"repository":{"full_name":"team/repo"},"push":{"changes":[{"new":{"type":"branch","name":"main","target":{"hash":"abc"}}}]}}`

type relayTestHost struct {
	pluginsdk.Host
	mu                sync.Mutex
	config            map[string]any
	state             map[string]any
	readErr, writeErr bool
	writes            int
	failWrite         int
}

func (h *relayTestHost) GetConfig(context.Context) (map[string]any, error) { return h.config, nil }

func (h *relayTestHost) GetState(context.Context, string, string, string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.readErr {
		return nil, false, errors.New("secret internal error")
	}
	return copyRelayState(h.state), h.state != nil, nil
}

func (h *relayTestHost) SetState(_ context.Context, _, _, _ string, v map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writes++
	if h.writeErr || h.writes == h.failWrite {
		return errors.New("secret internal error")
	}
	h.state = copyRelayState(v)
	return nil
}

func copyRelayState(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func newRelayHost(destination string) *relayTestHost {
	return &relayTestHost{config: map[string]any{"relay_enabled": true, "relay_product": "cloud", "relay_repository": "team/repo", "relay_event": "push", "relay_branches": "main", "relay_destination_url": destination + "/api/v1/automations/webhook/automation-1", "relay_signing_secret": "provider-secret", "relay_automation_secret": "automation-secret"}}
}

func relayRequest(body string) *pluginsdk.WebhookRequest {
	mac := hmac.New(sha256.New, []byte("provider-secret"))
	_, _ = mac.Write([]byte(body))
	return &pluginsdk.WebhookRequest{WebhookKey: "automation-relay", Method: "POST", Body: []byte(body), Headers: map[string]string{"X-Hub-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)), "X-Event-Key": "repo:push"}}
}

func relayCall(t *testing.T, w *Workflows, r *pluginsdk.WebhookRequest) *pluginsdk.WebhookResponse {
	t.Helper()
	out, err := w.HandleWebhook(context.Background(), r)
	require.NoError(t, err)
	require.NotNil(t, out)
	return out
}

func TestRelaySignedForwardAndPersistentDuplicates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/api/v1/automations/webhook/automation-1", r.URL.Path)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "automation-secret", r.Header.Get("X-Webhook-Secret"))
		require.Empty(t, r.Header.Get("X-Hub-Signature"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, relayPushBody, string(b))
		w.WriteHeader(200)
	}))
	defer server.Close()
	h := newRelayHost(server.URL)
	w := &Workflows{host: h}
	incoming := relayRequest(relayPushBody)
	incoming.Headers["Authorization"] = "Bearer provider-secret"
	incoming.Headers["Cookie"] = "session=private"
	require.EqualValues(t, 200, relayCall(t, w, incoming).Status)
	req := relayRequest(relayPushBody)
	req.Headers["X-Request-UUID"] = "different-unsigned-id"
	require.EqualValues(t, 200, relayCall(t, &Workflows{host: h}, req).Status)
	require.EqualValues(t, 1, calls.Load())
	state, _ := json.Marshal(h.state)
	require.NotContains(t, string(state), "secret")
	require.NotContains(t, string(state), relayPushBody)
	h.config["relay_automation_secret"] = "rotated"
	require.EqualValues(t, 200, relayCall(t, &Workflows{host: h}, req).Status)
	require.EqualValues(t, 1, calls.Load())
}

func TestRelayRejectsBeforeForwarding(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	cases := []struct {
		name   string
		status int
		change func(*relayTestHost, *pluginsdk.WebhookRequest)
	}{
		{"disabled", 404, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.config["relay_enabled"] = false }},
		{"method", 405, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { r.Method = "GET" }},
		{"unsigned", 401, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { delete(r.Headers, "X-Hub-Signature") }},
		{"tampered", 401, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { r.Body = append(r.Body, ' ') }},
		{"duplicate signature", 401, func(h *relayTestHost, r *pluginsdk.WebhookRequest) {
			r.Headers["x-hub-signature"] = r.Headers["X-Hub-Signature"]
		}},
		{"encoding", 415, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { r.Headers["Content-Encoding"] = "gzip" }},
		{"large body", 413, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { r.Body = make([]byte, 1024*1024+1) }},
		{"repository filter", 204, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.config["relay_repository"] = "other/repo" }},
		{"branch filter", 204, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.config["relay_branches"] = "release" }},
		{"event filter", 204, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { r.Headers["X-Event-Key"] = "pullrequest:created" }},
		{"invalid JSON", 400, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { *r = *relayRequest("{") }},
		{"destination path", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) {
			h.config["relay_destination_url"] = server.URL + "/other"
		}},
		{"destination query", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) {
			h.config["relay_destination_url"] = h.config["relay_destination_url"].(string) + "?token=bad"
		}},
		{"missing secret", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { delete(h.config, "relay_signing_secret") }},
		{"invalid product", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.config["relay_product"] = "invalid" }},
		{"CI branches", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.config["relay_event"] = "ci_result" }},
		{"state read failure", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.readErr = true }},
		{"state write failure", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.writeErr = true }},
		{"corrupt state", 503, func(h *relayTestHost, r *pluginsdk.WebhookRequest) { h.state = map[string]any{"unexpected": true} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRelayHost(server.URL)
			req := relayRequest(relayPushBody)
			tc.change(h, req)
			out := relayCall(t, &Workflows{host: h}, req)
			require.EqualValues(t, tc.status, out.Status)
			require.NotContains(t, string(out.Body), "secret internal error")
		})
	}
	require.Zero(t, calls.Load())
}

func TestRelayDoesNotRepeatUncertainOrRejectedForward(t *testing.T) {
	for _, status := range []int{302, 401, 429, 500, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
			}))
			defer server.Close()
			h := newRelayHost(server.URL)
			if status == 200 {
				h.failWrite = 2
			}
			require.EqualValues(t, 502, relayCall(t, &Workflows{host: h}, relayRequest(relayPushBody)).Status)
			require.EqualValues(t, 502, relayCall(t, &Workflows{host: h}, relayRequest(relayPushBody)).Status)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestRelayConcurrentDuplicate(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}))
	defer server.Close()
	h := newRelayHost(server.URL)
	w := &Workflows{host: h}
	results := make(chan int32, 2)
	go func() {
		r, _ := w.HandleWebhook(context.Background(), relayRequest(relayPushBody))
		results <- r.Status
	}()
	<-entered
	go func() {
		r, _ := w.HandleWebhook(context.Background(), relayRequest(relayPushBody))
		results <- r.Status
	}()
	close(release)
	require.EqualValues(t, 200, <-results)
	require.EqualValues(t, 200, <-results)
	require.EqualValues(t, 1, calls.Load())
}

func TestRelayAcceptsLowercaseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	req := relayRequest(relayPushBody)
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[strings.ToLower(k)] = v
	}
	req.Headers = headers
	require.EqualValues(t, 200, relayCall(t, &Workflows{host: newRelayHost(server.URL)}, req).Status)
}

func TestRelayProviderEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	cases := []struct{ product, event, kind, body string }{
		{"cloud", "pullrequest:created", "pull_request_opened", `{"repository":{"full_name":"team/repo"},"pullrequest":{"id":1,"state":"OPEN","destination":{"branch":{"name":"main"}}}}`},
		{"cloud", "pullrequest:fulfilled", "pull_request_merged", `{"repository":{"full_name":"team/repo"},"pullrequest":{"id":1,"state":"MERGED","destination":{"branch":{"name":"main"}}}}`},
		{"data_center", "repo:refs_changed", "push", `{"eventKey":"repo:refs_changed","repository":{"slug":"repo","project":{"key":"TEAM"}},"changes":[{"ref":{"type":"BRANCH","displayId":"main"},"type":"UPDATE","toHash":"abc"}]}`},
		{"data_center", "pr:opened", "pull_request_opened", `{"eventKey":"pr:opened","pullRequest":{"id":1,"state":"OPEN","toRef":{"displayId":"main","repository":{"slug":"repo","project":{"key":"TEAM"}}}}}`},
		{"data_center", "pr:merged", "pull_request_merged", `{"eventKey":"pr:merged","pullRequest":{"id":1,"state":"MERGED","toRef":{"displayId":"main","repository":{"slug":"repo","project":{"key":"TEAM"}}}}}`},
		{"cloud", "repo:commit_status_updated", "ci_result", `{"repository":{"full_name":"team/repo"},"commit_status":{"key":"build","state":"FAILED","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/team/repo/commit/9fec847784abb10b2fa567ee63b85bd238955d0e"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			h := newRelayHost(server.URL)
			h.config["relay_product"] = tc.product
			h.config["relay_event"] = tc.kind
			if tc.kind == "ci_result" {
				h.config["relay_branches"] = ""
				h.config["relay_conclusions"] = "failed"
			}
			req := relayRequest(tc.body)
			req.Headers["X-Event-Key"] = tc.event
			require.EqualValues(t, 200, relayCall(t, &Workflows{host: h}, req).Status)
			h.state = nil
			h.config["relay_repository"] = "foreign/repo"
			require.EqualValues(t, 204, relayCall(t, &Workflows{host: h}, req).Status)
			h.config["relay_repository"] = "team/repo"
			req.Headers["X-Event-Key"] = "incorrect"
			require.EqualValues(t, 204, relayCall(t, &Workflows{host: h}, req).Status)
		})
	}
}

func TestRelayReceiptCapacityAndExpiry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	h := newRelayHost(server.URL)
	ledger := relayLedger{Version: 1, Receipts: map[string]relayReceipt{}}
	for i := range relayMaxReceipts {
		id := sha256.Sum256([]byte(fmt.Sprint(i)))
		ledger.Receipts[hex.EncodeToString(id[:])] = relayReceipt{ClaimedAt: time.Now(), Outcome: "indeterminate"}
	}
	var err error
	h.state, err = encodeState(ledger)
	require.NoError(t, err)
	require.EqualValues(t, 503, relayCall(t, &Workflows{host: h}, relayRequest(relayPushBody)).Status)
	require.Zero(t, calls.Load())
	for id, receipt := range ledger.Receipts {
		receipt.ClaimedAt = time.Now().Add(-relayRetention - time.Minute)
		ledger.Receipts[id] = receipt
	}
	h.state, err = encodeState(ledger)
	require.NoError(t, err)
	require.EqualValues(t, 200, relayCall(t, &Workflows{host: h}, relayRequest(relayPushBody)).Status)
	var stored relayLedger
	require.NoError(t, decodeState(h.state, &stored))
	require.Len(t, stored.Receipts, 1)
}

func TestRelayLostHTTPResponseIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	h := newRelayHost(server.URL)
	require.EqualValues(t, 502, relayCall(t, &Workflows{host: h}, relayRequest(relayPushBody)).Status)
	require.EqualValues(t, 502, relayCall(t, &Workflows{host: h}, relayRequest(relayPushBody)).Status)
	require.EqualValues(t, 1, calls.Load())
}

func TestWebhookSignaturePublishedVector(t *testing.T) {
	// Atlassian's manage-webhooks documentation: unchanged UTF-8 bytes.
	body := []byte("Hello World!")
	signature := "sha256=a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9"
	require.True(t, verifyWebhookSignature(body, "It's a Secret to Everybody", signature))
	require.False(t, verifyWebhookSignature(append(body, ' '), "It's a Secret to Everybody", signature))
	require.False(t, verifyWebhookSignature(body, "wrong", signature))
	require.False(t, verifyWebhookSignature(body, "", signature))
}
