package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type WebhookEvent struct {
	Action string `json:"action"`

	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`

	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`

	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
}

type InstallationTokenResponse struct {
	Token string `json:"token"`
}

type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	Patch     string `json:"patch"`
}

func main() {
	http.HandleFunc("/webhook", handleWebhook)

	fmt.Println("listening on :80")
	if err := http.ListenAndServe(":80", nil); err != nil {
		panic(err)
	}
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	signature := r.Header.Get("X-Hub-Signature-256")

	fmt.Println("event:", eventType)
	fmt.Println("delivery:", deliveryID)

	if eventType != "pull_request" {
		fmt.Println("ignored non pull_request event")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ignored"))
		return
	}

	if !verifySignature(bodyBytes, signature, os.Getenv("GITHUB_WEBHOOK_SECRET")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var event WebhookEvent
	if err := json.Unmarshal(bodyBytes, &event); err != nil {
		http.Error(w, "failed to parse webhook json", http.StatusBadRequest)
		return
	}

	if event.Action != "opened" && event.Action != "synchronize" && event.Action != "reopened" {
		fmt.Println("ignored action:", event.Action)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ignored action"))
		return
	}

	if err := processPullRequest(event, deliveryID); err != nil {
		fmt.Println("error:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func processPullRequest(event WebhookEvent, deliveryID string) error {
	appID := mustGetEnv("GITHUB_APP_ID")
	privateKeyPath := mustGetEnv("GITHUB_PRIVATE_KEY_PATH")

	installationID := event.Installation.ID
	owner := event.Repository.Owner.Login
	repo := event.Repository.Name
	prNumber := event.PullRequest.Number

	appJWT, err := createAppJWT(appID, privateKeyPath)
	if err != nil {
		return fmt.Errorf("create app jwt: %w", err)
	}

	installationToken, err := createInstallationToken(appJWT, installationID)
	if err != nil {
		return fmt.Errorf("create installation token: %w", err)
	}

	files, err := fetchPRFiles(installationToken, owner, repo, prNumber)
	if err != nil {
		return fmt.Errorf("fetch pr files: %w", err)
	}

	comment := buildComment(event, deliveryID, files)

	if err := postPRComment(installationToken, owner, repo, prNumber, comment); err != nil {
		return fmt.Errorf("post pr comment: %w", err)
	}

	fmt.Println("posted comment to PR:", prNumber)
	return nil
}

func verifySignature(payload []byte, signatureHeader string, secret string) bool {
	if secret == "" {
		fmt.Println("warning: GITHUB_WEBHOOK_SECRET is empty")
		return false
	}

	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return false
	}

	expectedSig := strings.TrimPrefix(signatureHeader, "sha256=")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	actualSig := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(actualSig), []byte(expectedSig))
}

func createAppJWT(appID string, privateKeyPath string) (string, error) {
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", err
	}

	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM(keyBytes)
	if err != nil {
		return "", err
	}

	now := time.Now()

	claims := jwt.MapClaims{
		"iat": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(privateKey)
}

func createInstallationToken(appJWT string, installationID int64) (string, error) {
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)

	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	fmt.Println("installation token response:", string(bodyBytes))

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status=%d body=%s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp InstallationTokenResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return "", err
	}

	return tokenResp.Token, nil
}

func fetchPRFiles(token string, owner string, repo string, prNumber int) ([]PRFile, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/files", owner, repo, prNumber)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(bodyBytes))
	}

	var files []PRFile
	if err := json.Unmarshal(bodyBytes, &files); err != nil {
		return nil, err
	}

	return files, nil
}

func postPRComment(token string, owner string, repo string, prNumber int, comment string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, prNumber)

	reqBody := map[string]string{
		"body": comment,
	}

	bodyBytes, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(respBytes))
	}

	return nil
}

func buildComment(event WebhookEvent, deliveryID string, files []PRFile) string {
	var b strings.Builder

	b.WriteString("## Hello world from Gemini Code Review Agent\n\n")

	b.WriteString("This is an end-to-end GitHub App test comment.\n\n")

	b.WriteString("### PR Context\n\n")
	b.WriteString(fmt.Sprintf("- Repo: `%s/%s`\n", event.Repository.Owner.Login, event.Repository.Name))
	b.WriteString(fmt.Sprintf("- PR: `#%d`\n", event.PullRequest.Number))
	b.WriteString(fmt.Sprintf("- Title: `%s`\n", event.PullRequest.Title))
	b.WriteString(fmt.Sprintf("- Action: `%s`\n", event.Action))
	b.WriteString(fmt.Sprintf("- Delivery ID: `%s`\n", deliveryID))
	b.WriteString(fmt.Sprintf("- Base SHA: `%s`\n", event.PullRequest.Base.SHA))
	b.WriteString(fmt.Sprintf("- Head SHA: `%s`\n\n", event.PullRequest.Head.SHA))

	limit := 3
	if len(files) < limit {
		limit = len(files)
	}

	b.WriteString(fmt.Sprintf("### First %d file diffs\n\n", limit))

	for i := 0; i < limit; i++ {
		file := files[i]

		b.WriteString(fmt.Sprintf("#### Diff %d: `%s`\n\n", i+1, file.Filename))
		b.WriteString(fmt.Sprintf("- Status: `%s`\n", file.Status))
		b.WriteString(fmt.Sprintf("- Additions: `%d`\n", file.Additions))
		b.WriteString(fmt.Sprintf("- Deletions: `%d`\n\n", file.Deletions))

		patch := file.Patch
		if patch == "" {
			patch = "(No patch available. This may be a binary file or a very large diff.)"
		}

		patch = truncate(patch, 3000)

		b.WriteString("```diff\n")
		b.WriteString(patch)
		b.WriteString("\n```\n\n")
	}

	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}

	return s[:max] + "\n... truncated ..."
}

func mustGetEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		panic("missing env var: " + key)
	}
	return value
}