package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type LoggedRequest struct {
	ID              string            `json:"id"`
	ReceivedAt      string            `json:"received_at"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	RemoteAddr      string            `json:"remote_addr"`
	GitLabEvent     string            `json:"gitlab_event"`
	GitLabTokenSeen bool              `json:"gitlab_token_seen"`
	Headers         map[string]string `json:"headers"`
	RawBody         string            `json:"raw_body"`
	Result          string            `json:"result"`
	Error           string            `json:"error"`
}

var (
	mu       sync.Mutex
	requests []LoggedRequest
)

type GitLabMREvent struct {
	ObjectKind string `json:"object_kind"`
	EventType  string `json:"event_type"`

	Project struct {
		ID                int    `json:"id"`
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`

	ObjectAttributes struct {
		ID           int    `json:"id"`
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		State        string `json:"state"`
		Action       string `json:"action"`
		URL          string `json:"url"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`

	User struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
}

type GitLabMRDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	AMode       string `json:"a_mode"`
	BMode       string `json:"b_mode"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

func main() {
	http.HandleFunc("/", handleHome)
	http.HandleFunc("/api/requests", handleAPIRequests)

	http.HandleFunc("/webhooks/gitlab", handleGitLabWebhook)
	http.HandleFunc("/webhooks", handleGitLabWebhook)

	port := getenv("PORT", "80")

	fmt.Println("listening on :" + port)
	fmt.Println("frontend: http://localhost:" + port)
	fmt.Println("GitLab webhook URL: https://YOUR_NGROK_URL/webhooks/gitlab")

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		panic(err)
	}
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	t := template.Must(template.New("home").Parse(pageHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = t.Execute(w, nil)
}

func handleAPIRequests(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(requests)
}

func handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	logged := LoggedRequest{
		ID:              fmt.Sprintf("%d", time.Now().UnixNano()),
		ReceivedAt:      time.Now().Format(time.RFC3339),
		Method:          r.Method,
		Path:            r.URL.Path,
		RemoteAddr:      r.RemoteAddr,
		GitLabEvent:     r.Header.Get("X-Gitlab-Event"),
		GitLabTokenSeen: r.Header.Get("X-Gitlab-Token") != "",
		Headers:         headersToMap(r.Header),
		RawBody:         string(bodyBytes),
		Result:          "received",
	}

	addRequest(logged)

	if r.Method != http.MethodPost {
		updateRequest(logged.ID, "", "method not allowed")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !verifyGitLabWebhookToken(r.Header.Get("X-Gitlab-Token")) {
		updateRequest(logged.ID, "", "invalid X-Gitlab-Token")
		http.Error(w, "invalid X-Gitlab-Token", http.StatusUnauthorized)
		return
	}

	var event GitLabMREvent
	if err := json.Unmarshal(bodyBytes, &event); err != nil {
		updateRequest(logged.ID, "", "failed to parse GitLab webhook JSON: "+err.Error())
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}

	if event.ObjectKind != "merge_request" && event.EventType != "merge_request" {
		msg := "ignored non-merge-request event"
		updateRequest(logged.ID, msg, "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(msg))
		return
	}

	action := event.ObjectAttributes.Action
	if action != "open" && action != "update" && action != "reopen" {
		msg := "ignored MR action: " + action
		updateRequest(logged.ID, msg, "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(msg))
		return
	}

	if err := processMergeRequest(event); err != nil {
		updateRequest(logged.ID, "", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := fmt.Sprintf(
		"posted comment to GitLab MR !%d in project %s",
		event.ObjectAttributes.IID,
		event.Project.PathWithNamespace,
	)

	updateRequest(logged.ID, result, "")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func verifyGitLabWebhookToken(token string) bool {
	expected := os.Getenv("GITLAB_WEBHOOK_SECRET")
	if expected == "" {
		return true
	}
	return token == expected
}

func processMergeRequest(event GitLabMREvent) error {
	pat := mustGetEnv("GITLAB_PAT")
	baseURL := getenv("GITLAB_BASE_URL", "https://gitlab.com")

	projectID := event.Project.ID
	mrIID := event.ObjectAttributes.IID

	diffs, err := fetchMergeRequestDiffs(baseURL, pat, projectID, mrIID)
	if err != nil {
		return fmt.Errorf("fetch MR diffs: %w", err)
	}

	comment := buildComment(event, diffs)

	if err := postMergeRequestComment(baseURL, pat, projectID, mrIID, comment); err != nil {
		return fmt.Errorf("post MR comment: %w", err)
	}

	return nil
}

func fetchMergeRequestDiffs(baseURL string, pat string, projectID int, mrIID int) ([]GitLabMRDiff, error) {
	apiURL := fmt.Sprintf(
		"%s/api/v4/projects/%d/merge_requests/%d/diffs",
		strings.TrimRight(baseURL, "/"),
		projectID,
		mrIID,
	)

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("PRIVATE-TOKEN", pat)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitLab fetch diffs failed: status=%d body=%s", resp.StatusCode, string(bodyBytes))
	}

	var diffs []GitLabMRDiff
	if err := json.Unmarshal(bodyBytes, &diffs); err != nil {
		return nil, err
	}

	return diffs, nil
}

func postMergeRequestComment(baseURL string, pat string, projectID int, mrIID int, comment string) error {
	apiURL := fmt.Sprintf(
		"%s/api/v4/projects/%d/merge_requests/%d/notes",
		strings.TrimRight(baseURL, "/"),
		projectID,
		mrIID,
	)

	form := url.Values{}
	form.Set("body", comment)

	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}

	req.Header.Set("PRIVATE-TOKEN", pat)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("GitLab post note failed: status=%d body=%s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

func buildComment(event GitLabMREvent, diffs []GitLabMRDiff) string {
	var b strings.Builder

	b.WriteString("## hello world from GitLab Review Agent\n\n")
	b.WriteString("This comment was posted by the local Go webhook server using a GitLab PAT.\n\n")

	b.WriteString("### MR Context\n\n")
	b.WriteString(fmt.Sprintf("- Project: `%s`\n", event.Project.PathWithNamespace))
	b.WriteString(fmt.Sprintf("- MR: `!%d`\n", event.ObjectAttributes.IID))
	b.WriteString(fmt.Sprintf("- Title: `%s`\n", event.ObjectAttributes.Title))
	b.WriteString(fmt.Sprintf("- Action: `%s`\n", event.ObjectAttributes.Action))
	b.WriteString(fmt.Sprintf("- Source branch: `%s`\n", event.ObjectAttributes.SourceBranch))
	b.WriteString(fmt.Sprintf("- Target branch: `%s`\n", event.ObjectAttributes.TargetBranch))
	b.WriteString(fmt.Sprintf("- Last commit: `%s`\n\n", event.ObjectAttributes.LastCommit.ID))

	limit := 3
	if len(diffs) < limit {
		limit = len(diffs)
	}

	b.WriteString("### hello world + diff1, diff2, diff3\n\n")
	b.WriteString(fmt.Sprintf("Showing first `%d` diffs.\n\n", limit))

	for i := 0; i < limit; i++ {
		d := diffs[i]

		path := d.NewPath
		if path == "" {
			path = d.OldPath
		}

		b.WriteString(fmt.Sprintf("#### diff%d: `%s`\n\n", i+1, path))
		b.WriteString(fmt.Sprintf("- new_file: `%t`\n", d.NewFile))
		b.WriteString(fmt.Sprintf("- renamed_file: `%t`\n", d.RenamedFile))
		b.WriteString(fmt.Sprintf("- deleted_file: `%t`\n\n", d.DeletedFile))

		diffText := d.Diff
		if diffText == "" {
			diffText = "(No diff text available.)"
		}

		diffText = truncate(diffText, 2500)

		b.WriteString("```diff\n")
		b.WriteString(diffText)
		b.WriteString("\n```\n\n")
	}

	return b.String()
}

func headersToMap(h http.Header) map[string]string {
	m := make(map[string]string)
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}

func addRequest(req LoggedRequest) {
	mu.Lock()
	defer mu.Unlock()

	requests = append([]LoggedRequest{req}, requests...)

	if len(requests) > 20 {
		requests = requests[:20]
	}
}

func updateRequest(id string, result string, errMsg string) {
	mu.Lock()
	defer mu.Unlock()

	for i := range requests {
		if requests[i].ID == id {
			if result != "" {
				requests[i].Result = result
			}
			if errMsg != "" {
				requests[i].Error = errMsg
			}
			return
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... truncated ..."
}

func getenv(key string, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("missing env var: " + key)
	}
	return v
}

const pageHTML = `
<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>GitLab Webhook Inspector</title>
  <style>
    body {
      font-family: system-ui, -apple-system, BlinkMacSystemFont, sans-serif;
      margin: 24px;
      background: #f6f8fa;
    }
    h1 {
      margin-bottom: 4px;
    }
    .hint {
      color: #57606a;
      margin-bottom: 20px;
    }
    .request {
      background: white;
      border: 1px solid #d0d7de;
      border-radius: 8px;
      padding: 16px;
      margin-bottom: 16px;
    }
    .meta {
      display: grid;
      grid-template-columns: 170px 1fr;
      gap: 6px 12px;
      margin-bottom: 12px;
      font-size: 14px;
    }
    .key {
      font-weight: 600;
      color: #24292f;
    }
    .value {
      color: #57606a;
      word-break: break-all;
    }
    pre {
      background: #0d1117;
      color: #c9d1d9;
      padding: 12px;
      border-radius: 6px;
      overflow: auto;
      max-height: 420px;
      font-size: 12px;
    }
    .ok {
      color: #1a7f37;
      font-weight: 600;
    }
    .err {
      color: #cf222e;
      font-weight: 600;
    }
    button {
      padding: 8px 12px;
      border: 1px solid #d0d7de;
      border-radius: 6px;
      background: white;
      cursor: pointer;
      margin-bottom: 16px;
    }
  </style>
</head>
<body>
  <h1>GitLab Webhook Inspector</h1>
  <div class="hint">
    Set GitLab Merge Request webhook URL to <code>/webhook/gitlab</code>. This page shows payload, headers, processing result, and errors.
  </div>

  <button onclick="loadRequests()">Refresh</button>

  <div id="requests"></div>

  <script>
    async function loadRequests() {
      const res = await fetch('/api/requests');
      const requests = await res.json();

      const root = document.getElementById('requests');

      if (!requests || requests.length === 0) {
        root.innerHTML = '<p>No webhook requests received yet.</p>';
        return;
      }

      root.innerHTML = requests.map(req => {
        const prettyBody = prettyJSON(req.raw_body);
        const prettyHeaders = JSON.stringify(req.headers, null, 2);

        return ` + "`" + `
          <div class="request">
            <div class="meta">
              <div class="key">Received</div>
              <div class="value">${escapeHTML(req.received_at)}</div>

              <div class="key">Method / Path</div>
              <div class="value">${escapeHTML(req.method)} ${escapeHTML(req.path)}</div>

              <div class="key">GitLab Event</div>
              <div class="value">${escapeHTML(req.gitlab_event || '')}</div>

              <div class="key">GitLab Token Seen</div>
              <div class="value">${escapeHTML(String(req.gitlab_token_seen))}</div>

              <div class="key">Result</div>
              <div class="value ok">${escapeHTML(req.result || '')}</div>

              <div class="key">Error</div>
              <div class="value err">${escapeHTML(req.error || '')}</div>
            </div>

            <details open>
              <summary>Payload</summary>
              <pre>${escapeHTML(prettyBody)}</pre>
            </details>

            <details>
              <summary>Headers</summary>
              <pre>${escapeHTML(prettyHeaders)}</pre>
            </details>
          </div>
        ` + "`" + `;
      }).join('');
    }

    function prettyJSON(raw) {
      try {
        return JSON.stringify(JSON.parse(raw), null, 2);
      } catch (e) {
        return raw || '';
      }
    }

    function escapeHTML(value) {
      return String(value)
        .replaceAll('&', '&amp;')
        .replaceAll('<', '&lt;')
        .replaceAll('>', '&gt;')
        .replaceAll('"', '&quot;')
        .replaceAll("'", '&#039;');
    }

    loadRequests();
    setInterval(loadRequests, 3000);
  </script>
</body>
</html>
`