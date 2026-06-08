package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type CreateWebhookRequest struct {
	URL                   string `json:"url"`
	Token                 string `json:"token,omitempty"`
	MergeRequestsEvents  bool   `json:"merge_requests_events"`
	PushEvents            bool   `json:"push_events"`
	TagPushEvents         bool   `json:"tag_push_events"`
	NoteEvents            bool   `json:"note_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

type WebhookResponse struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseURL := getEnv("GITLAB_BASE_URL", "https://gitlab.com")
	pat := mustGetEnv("GITLAB_PAT")
	project := mustGetEnv("GITLAB_PROJECT")
	webhookURL := mustGetEnv("WEBHOOK_URL")
	webhookSecret := mustGetEnv("WEBHOOK_SECRET")

	hook, err := createGitLabProjectWebhook(ctx, baseURL, pat, project, webhookURL, webhookSecret)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Println("GitLab webhook created successfully")
	fmt.Println("hook_id:", hook.ID)
	fmt.Println("hook_url:", hook.URL)
}

func createGitLabProjectWebhook(
	ctx context.Context,
	baseURL string,
	pat string,
	project string,
	webhookURL string,
	webhookSecret string,
) (*WebhookResponse, error) {
	endpoint := fmt.Sprintf(
		"%s/api/v4/projects/%s/hooks",
		strings.TrimRight(baseURL, "/"),
		url.PathEscape(project),
	)

	body := CreateWebhookRequest{
		URL:                   webhookURL,
		Token:                 webhookSecret,
		MergeRequestsEvents:  true,
		PushEvents:            false,
		TagPushEvents:         false,
		NoteEvents:            false,
		EnableSSLVerification: true,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("PRIVATE-TOKEN", pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call GitLab API: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitLab API returned %s: %s", resp.Status, string(respBody))
	}

	var hook WebhookResponse
	if err := json.Unmarshal(respBody, &hook); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &hook, nil
}

func mustGetEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		fmt.Fprintf(os.Stderr, "missing required env var: %s\n", key)
		os.Exit(1)
	}
	return value
}

func getEnv(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}