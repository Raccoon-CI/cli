package ghrunner

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
	"strings"
)

// GitHub can tell the service what happens instead of being asked every second:
// a repository webhook for the workflow_job event says at once that a job is
// queued, and when it is taken and done. Listing the repositories stays, every
// SafetyPoll, for a delivery that never comes.

// maxEventBytes caps a delivery; a workflow_job payload is a few KB.
const maxEventBytes = 5 << 20

// Webhook receives GitHub's deliveries for a Service. Every request must carry
// the signature of its body made with Secret (X-Hub-Signature-256); nothing is
// read from an unsigned one.
type Webhook struct {
	Secret []byte
	S      *Service
}

// validSignature checks "sha256=<hex hmac of body>" in constant time.
func validSignature(secret, body []byte, header string) bool {
	sum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sum)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// jobEvent is the part of a workflow_job delivery the service uses. The job's
// fields are named as in the REST API.
type jobEvent struct {
	Action      string `json:"action"`
	WorkflowJob Job    `json:"workflow_job"`
	Repository  struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		var ev PushEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if !h.S.HandlePush(ev) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	case "pull_request":
		var ev PullRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if !h.S.HandlePullRequest(ev) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	case "workflow_job":
		var ev jobEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if !h.S.HandleJobEvent(ev.Repository.FullName, ev.Action, ev.WorkflowJob) {
			// not a repository we watch, or the service is not running (yet): GitHub
			// would redeliver on an error, and the listing finds the job anyway
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	// ping, and any event we did not ask for
	w.WriteHeader(http.StatusNoContent)
}

// HandleJobEvent takes a workflow_job event. It reports false when the service
// does not watch the repository or is not running.
func (s *Service) HandleJobEvent(repo, action string, j Job) bool {
	s.mu.Lock()
	ctx := s.runCtx
	var wake chan struct{}
	for r, ch := range s.wake {
		if strings.EqualFold(r, repo) {
			repo, wake = r, ch
			break
		}
	}
	s.mu.Unlock()
	if ctx == nil || wake == nil {
		return false
	}
	j.Repo = repo
	switch action {
	case "queued":
		s.consider(ctx, j)
		s.noteQueued(ctx, repo, j)
	case "in_progress", "completed":
		// Clears the promise a waiting runner made for the job, and lets the
		// loop look at the run (durations, jobs that follow).
		s.consider(ctx, j)
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	return true
}

// noteQueued tells the keep-alive policy that a run asked for our runners, once
// per run. The delivery does not say what started the run, and a scheduled run
// says nothing about activity, so the run is asked about.
func (s *Service) noteQueued(ctx context.Context, repo string, j Job) {
	if s.KeepAlive == nil || !j.HasLabelPrefix(s.prefix()) {
		return
	}
	s.mu.Lock()
	seen := s.arrived[j.RunID]
	s.arrived[j.RunID] = true
	s.mu.Unlock()
	if seen {
		return
	}
	s.wg.Go(func() {
		if info, err := s.GH.GetRun(ctx, repo, j.RunID); err == nil && info.Event == "schedule" {
			return
		}
		s.KeepAlive.Arrival(s.poolOf(repo), j.CreatedAt)
	})
}

// Hook is a repository webhook, as far as the service cares.
type Hook struct {
	ID     int64 `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

func hookBody(url, secret string) map[string]any {
	return map[string]any{
		"name": "web", "active": true, "events": []string{"workflow_job", "push", "pull_request"},
		"config": map[string]any{"url": url, "content_type": "json", "secret": secret, "insecure_ssl": "0"},
	}
}

// Hooks lists a repository's webhooks (needs admin rights on it).
func (c *Client) Hooks(ctx context.Context, repo string) ([]Hook, error) {
	var hs []Hook
	err := c.do(ctx, "GET", "/repos/"+repo+"/hooks?per_page=100", nil, &hs)
	return hs, err
}

// SetHook makes the repository deliver workflow_job events to url, signed with
// secret: the hook already pointing at url is updated, else one is created. It
// returns the hook's id.
func (c *Client) SetHook(ctx context.Context, repo, url, secret string) (int64, error) {
	hs, err := c.Hooks(ctx, repo)
	if err != nil {
		return 0, err
	}
	for _, h := range hs {
		if h.Config.URL == url {
			err := c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/hooks/%d", repo, h.ID), hookBody(url, secret), nil)
			return h.ID, err
		}
	}
	var h Hook
	err = c.do(ctx, "POST", "/repos/"+repo+"/hooks", hookBody(url, secret), &h)
	return h.ID, err
}

// DeleteHook removes a webhook; one that is gone already is fine.
func (c *Client) DeleteHook(ctx context.Context, repo string, id int64) error {
	err := c.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/hooks/%d", repo, id), nil, nil)
	if errors.Is(err, NotFound) {
		return nil
	}
	return err
}

// RegisterHooks points every repository's webhook at url. The returned function
// deletes them again; call it when the service stops, because the address is
// not meant to outlive it. All or nothing: on an error the ones made are removed.
func RegisterHooks(ctx context.Context, gh *Client, repos []string, url, secret string) (func(context.Context), error) {
	ids := map[string]int64{}
	remove := func(ctx context.Context) {
		for repo, id := range ids {
			gh.DeleteHook(ctx, repo, id)
		}
	}
	for _, repo := range repos {
		id, err := gh.SetHook(ctx, repo, url, secret)
		if err != nil {
			remove(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("webhook of %s: %w", repo, err)
		}
		ids[repo] = id
	}
	return remove, nil
}
