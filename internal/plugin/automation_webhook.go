package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/kandev/kandev/pkg/pluginsdk"
	"kandev-plugin-bitbucket/internal/domain"
	"slices"
	"strings"
)

type automationWebhookConfig struct {
	Repository  string   `json:"repository"`
	Branches    []string `json:"branches,omitempty"`
	Conclusions []string `json:"conclusions,omitempty"`
}

func (r *Runtime) DescribeAutomationCondition(ctx context.Context, req *pluginsdk.AutomationConditionRequest) (*pluginsdk.AutomationConditionResponse, error) {
	w, err := r.ready()
	if err != nil {
		return nil, err
	}
	return w.DescribeAutomationCondition(ctx, req)
}
func (r *Runtime) VerifyAutomationWebhook(ctx context.Context, req *pluginsdk.AutomationWebhookRequest) (*pluginsdk.AutomationWebhookResponse, error) {
	w, err := r.ready()
	if err != nil {
		return nil, err
	}
	return w.VerifyAutomationWebhook(ctx, req)
}
func (w *Workflows) DescribeAutomationCondition(ctx context.Context, req *pluginsdk.AutomationConditionRequest) (*pluginsdk.AutomationConditionResponse, error) {
	unavailable := &pluginsdk.AutomationConditionResponse{Reason: "connection_unavailable"}
	if req == nil || req.WorkspaceId == "" {
		return unavailable, nil
	}
	loader, ok := w.resolver.(interface {
		Load(context.Context, string) (ConnectionSettings, bool, error)
	})
	if !ok {
		return unavailable, nil
	}
	connection, found, err := loader.Load(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	if !found || connection.ConnectionBinding == "" {
		return unavailable, nil
	}
	switch req.ConditionKey {
	case "pull_request_opened", "pull_request_merged", "push":
	case "ci_result":
		if connection.Product != domain.ProductCloud {
			return &pluginsdk.AutomationConditionResponse{Reason: "ci_webhook_not_supported"}, nil
		}
	default:
		return &pluginsdk.AutomationConditionResponse{Reason: "condition_not_supported"}, nil
	}
	if len(req.Config) > 0 {
		var cfg automationWebhookConfig
		if json.Unmarshal(req.Config, &cfg) != nil || strings.Count(cfg.Repository, "/") != 1 {
			return &pluginsdk.AutomationConditionResponse{Reason: "repository_required"}, nil
		}
		for _, conclusion := range cfg.Conclusions {
			if !slices.Contains([]string{"successful", "failed", "stopped"}, conclusion) {
				return &pluginsdk.AutomationConditionResponse{Reason: "invalid_conclusion"}, nil
			}
		}
		if req.ConditionKey == "ci_result" && len(cfg.Branches) > 0 {
			return &pluginsdk.AutomationConditionResponse{Reason: "ci_branch_filter_not_supported"}, nil
		}
		provider, err := w.provider(ctx, req.WorkspaceId)
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(cfg.Repository, "/", 2)
		if parts[0] == "" || parts[1] == "" {
			return &pluginsdk.AutomationConditionResponse{Reason: "repository_required"}, nil
		}
		if _, err = hydrateRepositoryIdentity(ctx, provider, domain.Repository{Namespace: parts[0], Slug: parts[1]}); err != nil {
			return &pluginsdk.AutomationConditionResponse{Reason: "repository_unavailable"}, nil
		}
	}
	response := &pluginsdk.AutomationConditionResponse{Available: true, ConnectionId: connection.ConnectionBinding, ConnectionRevision: fmt.Sprintf("%s:%d", connection.ConnectionBinding, connection.CredentialGeneration)}
	if len(req.Config) == 0 {
		provider, err := w.provider(ctx, req.WorkspaceId)
		if err != nil {
			return nil, err
		}
		repos, err := provider.ListRepositories(ctx, "", 100)
		if err != nil {
			return nil, err
		}
		values := []string{}
		for _, repo := range repos {
			if len(values) >= 100 {
				break
			}
			values = append(values, repo.Namespace+"/"+repo.Slug)
		}
		response.ConfigOptions, _ = json.Marshal(map[string][]string{"repository": values})
	}
	return response, nil
}
func (w *Workflows) VerifyAutomationWebhook(ctx context.Context, req *pluginsdk.AutomationWebhookRequest) (*pluginsdk.AutomationWebhookResponse, error) {
	rejected := &pluginsdk.AutomationWebhookResponse{Outcome: "rejected"}
	if req == nil || !verifyWebhookSignature(req.Body, req.Secret, req.Headers["x-hub-signature"]) {
		return rejected, nil
	}
	description, err := w.DescribeAutomationCondition(ctx, &pluginsdk.AutomationConditionRequest{WorkspaceId: req.WorkspaceId, ConditionKey: req.ConditionKey, Config: req.Config})
	if err != nil {
		return nil, err
	}
	if !description.Available || description.ConnectionId != req.ConnectionId || description.ConnectionRevision != req.ConnectionRevision {
		return rejected, nil
	}
	identity, _, err := connectionIdentityForResolver(ctx, w.resolver, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	var cfg automationWebhookConfig
	if err = json.Unmarshal(req.Config, &cfg); err != nil {
		return rejected, nil
	}
	data, matched, err := matchWebhookPayload(identity.Product, req.ConditionKey, req.Headers["x-event-key"], req.Body, cfg)
	if err != nil {
		return &pluginsdk.AutomationWebhookResponse{Outcome: "malformed"}, nil
	}
	if !matched {
		return &pluginsdk.AutomationWebhookResponse{Outcome: "ignored"}, nil
	}
	return &pluginsdk.AutomationWebhookResponse{Outcome: "accepted", EventKind: req.ConditionKey, Data: data}, nil
}
func verifyWebhookSignature(body []byte, secret, signature string) bool {
	if secret == "" || !strings.HasPrefix(signature, "sha256=") || len(signature) != 71 {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(digest) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(digest, mac.Sum(nil))
}
