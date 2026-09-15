package plugin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"kandev-plugin-bitbucket/internal/domain"
)

// webhookObject only reads provider payloads; no field can select host authority.
type webhookObject map[string]any

func objectAt(obj webhookObject, keys ...string) webhookObject {
	for _, key := range keys {
		value, ok := obj[key].(map[string]any)
		if !ok {
			return nil
		}
		obj = value
	}
	return obj
}

func textAt(obj webhookObject, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	parent := objectAt(obj, keys[:len(keys)-1]...)
	value, _ := parent[keys[len(keys)-1]].(string)
	return value
}

func listAt(obj webhookObject, key string) []any { value, _ := obj[key].([]any); return value }

func branchMatches(filters []string, branch string) bool {
	return branch != "" && (len(filters) == 0 || slices.Contains(filters, branch))
}

func matchWebhookPayload(product domain.Product, kind, event string, body []byte, cfg relayPayloadFilter) (bool, error) {
	var payload webhookObject
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return false, fmt.Errorf("invalid event object")
	}
	if product == domain.ProductDataCenter {
		signedEvent := textAt(payload, "eventKey")
		if signedEvent == "" || (event != "" && signedEvent != event) {
			return false, nil
		}
		event = signedEvent
	}
	repo := textAt(payload, "repository", "full_name")
	if product == domain.ProductDataCenter {
		repository := objectAt(payload, "repository")
		if repository == nil {
			repository = objectAt(payload, "pullRequest", "toRef", "repository")
		}
		repo = textAt(repository, "project", "key") + "/" + textAt(repository, "slug")
	}
	if !strings.EqualFold(repo, cfg.Repository) {
		return false, nil
	}
	matched := false
	switch kind {
	case "pull_request_opened", "pull_request_merged":
		pr := objectAt(payload, "pullrequest")
		branch := textAt(pr, "destination", "branch", "name")
		expected := "pullrequest:created"
		state := "OPEN"
		if kind == "pull_request_merged" {
			expected = "pullrequest:fulfilled"
			state = "MERGED"
		}
		if product == domain.ProductDataCenter {
			pr = objectAt(payload, "pullRequest")
			branch = textAt(pr, "toRef", "displayId")
			expected = "pr:opened"
			if kind == "pull_request_merged" {
				expected = "pr:merged"
			}
		}
		id, _ := pr["id"].(float64)
		matched = event == expected && textAt(pr, "state") == state && id > 0 && branchMatches(cfg.Branches, branch)
	case "push":
		expected := "repo:push"
		changes := listAt(objectAt(payload, "push"), "changes")
		if product == domain.ProductDataCenter {
			expected = "repo:refs_changed"
			changes = listAt(payload, "changes")
		}
		if event != expected {
			return false, nil
		}
		for _, item := range changes {
			change, ok := item.(map[string]any)
			if !ok {
				continue
			}
			branch := textAt(change, "new", "name")
			valid := textAt(change, "new", "type") == "branch" && textAt(change, "new", "target", "hash") != ""
			if product == domain.ProductDataCenter {
				branch = textAt(change, "ref", "displayId")
				valid = textAt(change, "ref", "type") == "BRANCH" && textAt(change, "type") != "DELETE" && textAt(change, "toHash") != ""
			}
			if valid && branchMatches(cfg.Branches, branch) {
				matched = true
			}
		}
	case "ci_result":
		if product != domain.ProductCloud || len(cfg.Branches) > 0 {
			return false, nil
		}
		status := objectAt(payload, "commit_status")
		state := textAt(status, "state")
		terminal := slices.Contains([]string{"SUCCESSFUL", "FAILED", "STOPPED"}, state)
		matched = (event == "repo:commit_status_created" || event == "repo:commit_status_updated") && terminal && textAt(status, "key") != "" && cloudStatusCommit(status, repo) != "" && (len(cfg.Conclusions) == 0 || slices.Contains(cfg.Conclusions, strings.ToLower(state)))
	}
	if !matched {
		return false, nil
	}
	return true, nil
}

// Cloud webhook statuses identify the commit through links.commit, not commit.hash.
// Parse only; a provider URL never causes an outbound request here.
var webhookCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func cloudStatusCommit(status webhookObject, repository string) string {
	link, err := url.Parse(textAt(status, "links", "commit", "href"))
	if err != nil || link.Host != "api.bitbucket.org" || (link.Scheme != "https" && link.Scheme != "http") || link.User != nil {
		return ""
	}
	prefix := "/2.0/repositories/" + repository + "/commit/"
	if !strings.HasPrefix(link.Path, prefix) {
		return ""
	}
	hash := strings.TrimPrefix(link.Path, prefix)
	if !webhookCommitPattern.MatchString(hash) {
		return ""
	}
	return hash
}
