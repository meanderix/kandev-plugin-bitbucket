package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"kandev-plugin-bitbucket/internal/domain"
	"testing"
)

func TestWebhookHMACPublishedVector(t *testing.T) {
	require.True(t, verifyWebhookSignature([]byte("Hello World!"), "It's a Secret to Everybody", "sha256=a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9"))
	for _, signature := range []string{"", "sha1=123", "sha256=wrong", "sha256="} {
		require.False(t, verifyWebhookSignature([]byte("Hello World!"), "secret", signature))
	}
	body := []byte("{\n \"message\": \"こんにちは\"\n}")
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	require.True(t, verifyWebhookSignature(body, "secret", signature))
	require.False(t, verifyWebhookSignature(append(body, ' '), "secret", signature))
	require.False(t, verifyWebhookSignature(body, "wrong", signature))
	require.False(t, verifyWebhookSignature(body, "", signature))
}
func TestWebhookPayloadMatching(t *testing.T) {
	for _, tc := range []struct {
		name              string
		product           domain.Product
		event, kind, body string
	}{
		{"cloud push", domain.ProductCloud, "repo:push", "push", `{"repository":{"full_name":"team/repo"},"push":{"changes":[{"new":{"type":"branch","name":"main","target":{"hash":"abc"}}}]}}`},
		{"cloud PR", domain.ProductCloud, "pullrequest:created", "pull_request_opened", `{"repository":{"full_name":"team/repo"},"pullrequest":{"id":1,"state":"OPEN","destination":{"branch":{"name":"main"}}}}`},
		{"cloud merge", domain.ProductCloud, "pullrequest:fulfilled", "pull_request_merged", `{"repository":{"full_name":"team/repo"},"pullrequest":{"id":1,"state":"MERGED","destination":{"branch":{"name":"main"}}}}`},
		{"dc push", domain.ProductDataCenter, "repo:refs_changed", "push", `{"eventKey":"repo:refs_changed","repository":{"slug":"repo","project":{"key":"TEAM"}},"changes":[{"ref":{"type":"BRANCH","displayId":"main"},"type":"UPDATE","toHash":"abc"}]}`},
		{"dc PR", domain.ProductDataCenter, "pr:opened", "pull_request_opened", `{"eventKey":"pr:opened","pullRequest":{"id":1,"state":"OPEN","toRef":{"displayId":"main","repository":{"slug":"repo","project":{"key":"TEAM"}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "team/repo"
			if tc.product == domain.ProductDataCenter {
				repo = "TEAM/repo"
			}
			cfg := automationWebhookConfig{Repository: repo, Branches: []string{"main"}}
			data, match, err := matchWebhookPayload(tc.product, tc.kind, tc.event, []byte(tc.body), cfg)
			require.NoError(t, err)
			require.True(t, match)
			require.NotEmpty(t, data)
			cfg.Branches = []string{"other"}
			_, match, err = matchWebhookPayload(tc.product, tc.kind, tc.event, []byte(tc.body), cfg)
			require.NoError(t, err)
			require.False(t, match)
			cfg.Repository = "foreign/repo"
			cfg.Branches = nil
			_, match, err = matchWebhookPayload(tc.product, tc.kind, tc.event, []byte(tc.body), cfg)
			require.NoError(t, err)
			require.False(t, match)
		})
	}
}

func TestWebhookCloudCIAndDataCenterMerge(t *testing.T) {
	ci := []byte(`{"repository":{"full_name":"team/repo"},"commit_status":{"key":"build","state":"FAILED","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/team/repo/commit/9fec847784abb10b2fa567ee63b85bd238955d0e"}}}}`)
	cfg := automationWebhookConfig{Repository: "team/repo", Conclusions: []string{"failed"}}
	_, matched, err := matchWebhookPayload(domain.ProductCloud, "ci_result", "repo:commit_status_updated", ci, cfg)
	require.NoError(t, err)
	require.True(t, matched)
	cfg.Conclusions = []string{"successful"}
	_, matched, err = matchWebhookPayload(domain.ProductCloud, "ci_result", "repo:commit_status_updated", ci, cfg)
	require.NoError(t, err)
	require.False(t, matched)
	cfg.Conclusions = nil
	cfg.Branches = []string{"main"}
	_, matched, err = matchWebhookPayload(domain.ProductCloud, "ci_result", "repo:commit_status_updated", ci, cfg)
	require.NoError(t, err)
	require.False(t, matched)
	dc := []byte(`{"eventKey":"pr:merged","pullRequest":{"id":1,"state":"MERGED","toRef":{"displayId":"main","repository":{"slug":"repo","project":{"key":"TEAM"}}}}}`)
	_, matched, err = matchWebhookPayload(domain.ProductDataCenter, "pull_request_merged", "pr:merged", dc, automationWebhookConfig{Repository: "TEAM/repo"})
	require.NoError(t, err)
	require.True(t, matched)
	_, matched, err = matchWebhookPayload(domain.ProductDataCenter, "pull_request_merged", "pr:opened", dc, automationWebhookConfig{Repository: "TEAM/repo"})
	require.NoError(t, err)
	require.False(t, matched)
}

func TestWorkflowsWebhookUsesBoundConnectionAndRepository(t *testing.T) {
	ctx := context.Background()
	resolver := &connectionSettingsResolver{staticResolver: staticResolver{provider: &workflowProvider{repositories: []domain.Repository{{Namespace: "team", Slug: "repo"}}}}, found: true, settings: ConnectionSettings{Product: domain.ProductCloud, ConnectionBinding: "connection-1", CredentialGeneration: 7}}
	workflows, err := NewWorkflows(newConnectionHost(), resolver)
	require.NoError(t, err)
	config := []byte(`{"repository":"team/repo","branches":["main"]}`)
	info, err := workflows.DescribeAutomationCondition(ctx, &pluginsdk.AutomationConditionRequest{WorkspaceId: "workspace-1", ConditionKey: "push", Config: config})
	require.NoError(t, err)
	require.True(t, info.Available)
	body := []byte(`{"repository":{"full_name":"team/repo"},"push":{"changes":[{"new":{"type":"branch","name":"main","target":{"hash":"abc"}}}]}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	req := &pluginsdk.AutomationWebhookRequest{WorkspaceId: "workspace-1", ConditionKey: "push", Config: config, ConnectionId: info.ConnectionId, ConnectionRevision: info.ConnectionRevision, Secret: "secret", Body: body, Headers: map[string]string{"x-hub-signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)), "x-event-key": "repo:push"}}
	response, err := workflows.VerifyAutomationWebhook(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "accepted", response.Outcome)
	resolver.settings.CredentialGeneration++
	response, err = workflows.VerifyAutomationWebhook(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "rejected", response.Outcome)
	resolver.settings.CredentialGeneration--
	req.Config = []byte(`{"repository":"foreign/repo"}`)
	response, err = workflows.VerifyAutomationWebhook(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "rejected", response.Outcome)
}
