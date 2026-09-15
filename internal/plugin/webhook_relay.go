package plugin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"kandev-plugin-bitbucket/internal/domain"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	relayStateKey    = "bitbucket.automation-relay.v1"
	relayMaxReceipts = 1000
	relayRetention   = 7 * 24 * time.Hour
)

type relayPayloadFilter struct {
	Repository  string
	Branches    []string
	Conclusions []string
}

type relayConfig struct {
	relayPayloadFilter
	Product          domain.Product
	Event            string
	Destination      string
	SigningSecret    string
	AutomationSecret string
}

type relayReceipt struct {
	ClaimedAt time.Time `json:"claimed_at"`
	Outcome   string    `json:"outcome"`
	Status    int       `json:"destination_status,omitempty"`
}

type relayLedger struct {
	Version  int                     `json:"version"`
	Receipts map[string]relayReceipt `json:"receipts"`
}

// One host-supervised process owns the ledger. Host state has atomic writes but
// no compare-and-swap; serialize read/claim/forward/complete within that process.
// A claim survives restart even if the HTTP result or completion write is lost.
func (w *Workflows) handleAutomationRelay(ctx context.Context, request *pluginsdk.WebhookRequest) *pluginsdk.WebhookResponse {
	if request.Method != http.MethodPost {
		return relayResponse(405, "method_not_allowed", "")
	}
	if len(request.Body) > 1<<20 {
		return relayResponse(413, "body_too_large", "")
	}
	if encoding, ok := relayHeader(request.Headers, "Content-Encoding"); !ok || (encoding != "" && encoding != "identity") {
		return relayResponse(415, "unsupported_encoding", "")
	}
	config, err := w.host.GetConfig(ctx)
	if err != nil {
		return relayResponse(503, "configuration_unavailable", "")
	}
	if enabled, _ := config["relay_enabled"].(bool); !enabled {
		return relayResponse(404, "relay_disabled", "")
	}
	cfg, err := parseRelayConfig(config)
	if err != nil {
		return relayResponse(503, "configuration_invalid", "")
	}
	signature, ok := relayHeader(request.Headers, "X-Hub-Signature")
	if !ok || !verifyWebhookSignature(request.Body, cfg.SigningSecret, signature) {
		return relayResponse(401, "invalid_signature", "")
	}
	event, ok := relayHeader(request.Headers, "X-Event-Key")
	if !ok {
		return relayResponse(400, "invalid_event_header", "")
	}
	matched, err := matchWebhookPayload(cfg.Product, cfg.Event, event, request.Body, cfg.relayPayloadFilter)
	if err != nil {
		return relayResponse(400, "invalid_payload", "")
	}
	if !matched {
		return relayResponse(204, "", "")
	}
	// Unsigned delivery headers cannot change identity. Credential rotation and
	// filter edits must not turn the same body/destination into a new delivery.
	digest := sha256.Sum256(append([]byte(cfg.Destination+"\x00"), request.Body...))
	id := hex.EncodeToString(digest[:])
	w.relayMu.Lock()
	defer w.relayMu.Unlock()
	if ctx.Err() != nil {
		return relayResponse(503, "request_cancelled", id)
	}
	ledger, err := w.loadRelayLedger(ctx, time.Now())
	if err != nil {
		return relayResponse(503, "receipt_store_unavailable", id)
	}
	if receipt, exists := ledger.Receipts[id]; exists {
		if receipt.Outcome == "forwarded" {
			return relayResponse(200, "duplicate", id)
		}
		return relayResponse(502, "delivery_requires_review", id)
	}
	if len(ledger.Receipts) >= relayMaxReceipts {
		return relayResponse(503, "receipt_capacity_reached", id)
	}
	receipt := relayReceipt{ClaimedAt: time.Now().UTC(), Outcome: "claimed"}
	ledger.Receipts[id] = receipt
	if err := w.saveRelayLedger(ctx, ledger); err != nil {
		return relayResponse(503, "receipt_store_unavailable", id)
	}
	receipt.Outcome = "indeterminate"
	receipt.Status, err = forwardRelay(ctx, cfg, request.Body)
	if err == nil {
		receipt.Outcome = "rejected"
		if receipt.Status >= 200 && receipt.Status < 300 {
			receipt.Outcome = "forwarded"
		}
	}
	ledger.Receipts[id] = receipt
	// A disconnected sender must not prevent saving an already received outcome.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := w.saveRelayLedger(saveCtx, ledger); err != nil {
		return relayResponse(502, "delivery_requires_review", id)
	}
	if receipt.Outcome != "forwarded" {
		return relayResponse(502, "delivery_requires_review", id)
	}
	return relayResponse(200, "forwarded", id)
}

func forwardRelay(ctx context.Context, cfg relayConfig, body []byte) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Destination, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Webhook-Secret", cfg.AutomationSecret)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_ = response.Body.Close() // Never return or log the destination's body/headers.
	return response.StatusCode, nil
}

func parseRelayConfig(config map[string]any) (relayConfig, error) {
	text := func(key string) string { v, _ := config[key].(string); return v }
	cfg := relayConfig{
		Product:          domain.Product(text("relay_product")),
		Event:            text("relay_event"),
		Destination:      text("relay_destination_url"),
		SigningSecret:    text("relay_signing_secret"),
		AutomationSecret: text("relay_automation_secret"),
	}
	cfg.Repository = strings.TrimSpace(text("relay_repository"))
	cfg.Branches = relayFilterList(text("relay_branches"))
	cfg.Conclusions = relayFilterList(strings.ToLower(text("relay_conclusions")))
	repoParts := strings.Split(cfg.Repository, "/")
	valid := (cfg.Product == domain.ProductCloud || cfg.Product == domain.ProductDataCenter) && slices.Contains([]string{"pull_request_opened", "pull_request_merged", "push", "ci_result"}, cfg.Event)
	valid = valid && len(repoParts) == 2 && repoParts[0] != "" && repoParts[1] != "" && !strings.ContainsAny(cfg.Repository, " \t\r\n?#")
	valid = valid && strings.TrimSpace(cfg.SigningSecret) != "" && strings.TrimSpace(cfg.AutomationSecret) != "" && !strings.ContainsAny(cfg.AutomationSecret, "\r\n")
	if cfg.Event == "ci_result" {
		valid = valid && cfg.Product == domain.ProductCloud && len(cfg.Branches) == 0
	} else {
		valid = valid && len(cfg.Conclusions) == 0
	}
	for _, conclusion := range cfg.Conclusions {
		valid = valid && slices.Contains([]string{"successful", "failed", "stopped"}, conclusion)
	}
	destination, err := url.Parse(cfg.Destination)
	if err != nil {
		return cfg, fmt.Errorf("invalid relay destination")
	}
	const prefix = "/api/v1/automations/webhook/"
	id := strings.TrimPrefix(destination.Path, prefix)
	valid = valid && (destination.Scheme == "http" || destination.Scheme == "https") &&
		destination.Hostname() != "" && destination.User == nil &&
		destination.RawQuery == "" && !destination.ForceQuery && destination.Fragment == "" &&
		destination.RawPath == "" && strings.HasPrefix(destination.Path, prefix) &&
		id != "" && !strings.ContainsAny(id, "/\\ \t\r\n") && id != "." && id != ".."
	if !valid {
		return cfg, fmt.Errorf("invalid relay configuration")
	}
	return cfg, nil
}

func relayFilterList(value string) []string {
	var result []string
	for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func relayHeader(headers map[string]string, name string) (string, bool) {
	value := ""
	found := false
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			if found || strings.Contains(v, ",") {
				return "", false
			}
			value = v
			found = true
		}
	}
	return value, true
}

func verifyWebhookSignature(body []byte, secret, signature string) bool {
	if secret == "" || !strings.HasPrefix(signature, "sha256=") || len(signature) != 71 {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(digest, mac.Sum(nil))
}

func (w *Workflows) loadRelayLedger(ctx context.Context, now time.Time) (relayLedger, error) {
	ledger := relayLedger{Version: 1, Receipts: map[string]relayReceipt{}}
	value, found, err := w.host.GetState(ctx, "instance", "", relayStateKey)
	if err != nil || !found {
		return ledger, err
	}
	ledger = relayLedger{}
	if err := decodeState(value, &ledger); err != nil {
		return ledger, err
	}
	if ledger.Version != 1 || ledger.Receipts == nil {
		return ledger, fmt.Errorf("invalid relay ledger")
	}
	for id, receipt := range ledger.Receipts {
		digest, err := hex.DecodeString(id)
		if err != nil || len(digest) != 32 || receipt.ClaimedAt.IsZero() || !slices.Contains([]string{"claimed", "forwarded", "rejected", "indeterminate"}, receipt.Outcome) {
			return ledger, fmt.Errorf("invalid relay receipt")
		}
		if !receipt.ClaimedAt.Add(relayRetention).After(now) {
			delete(ledger.Receipts, id)
		}
	}
	return ledger, nil
}

func (w *Workflows) saveRelayLedger(ctx context.Context, ledger relayLedger) error {
	value, err := encodeState(ledger)
	if err != nil {
		return err
	}
	return w.host.SetState(ctx, "instance", "", relayStateKey, value)
}

func relayResponse(status int32, outcome, id string) *pluginsdk.WebhookResponse {
	response := &pluginsdk.WebhookResponse{Status: status, Headers: map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}}
	if status == 405 {
		response.Headers["Allow"] = "POST"
	}
	if status != 204 {
		response.Body = []byte(fmt.Sprintf(`{"status":%q,"receipt":%q}`, outcome, id))
	}
	return response
}
