// Package ghrunner runs the jobs of GitHub Actions workflows on InstaBox
// machines: GitHub still triggers and orchestrates the workflow (so every
// workflow feature works as it does there), and a job whose `runs-on` names one
// of our labels is picked up by a runner we start on a fresh machine, for that
// job only. It is the shape Blacksmith, InstaBox's own runner and others have.
package ghrunner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is the little of GitHub's REST API the service needs. It remembers
// the ETag of every GET and sends it back: a 304 costs no rate limit, which
// lets the service poll often.
type Client struct {
	Base  string // default https://api.github.com
	Token string
	HTTP  *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	etag string
	body []byte
}

// NotFound is returned for a 404.
var NotFound = errors.New("not found")

func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return "https://api.github.com"
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	c.mu.Lock()
	prev, haveETag := c.cache[path]
	c.mu.Unlock()
	if method == "GET" && haveETag {
		req.Header.Set("If-None-Match", prev.etag)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && haveETag:
		data = prev.body
	case resp.StatusCode == http.StatusNotFound:
		return NotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data[:min(len(data), 300)])))
	case method == "GET" && resp.Header.Get("ETag") != "":
		c.mu.Lock()
		if c.cache == nil {
			c.cache = map[string]cached{}
		}
		c.cache[path] = cached{resp.Header.Get("ETag"), data}
		c.mu.Unlock()
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Run is a workflow run.
type Run struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Event  string `json:"event"`
}

// Job is one job of a run.
type Job struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"` // queued, in_progress, completed
	Conclusion  string    `json:"conclusion"`
	Labels      []string  `json:"labels"`
	RunnerName  string    `json:"runner_name"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	// Repo is the repository the job was found in (set by the service).
	Repo string `json:"-"`
}

// ActiveRuns lists the runs that are queued or in progress: the ones that can
// have a job waiting for a runner.
func (c *Client) ActiveRuns(ctx context.Context, repo string) ([]Run, error) {
	var out []Run
	for _, st := range []string{"queued", "in_progress"} {
		var page struct {
			Runs []Run `json:"workflow_runs"`
		}
		if err := c.do(ctx, "GET", "/repos/"+repo+"/actions/runs?status="+st+"&per_page=50", nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Runs...)
	}
	return out, nil
}

// RunJobs lists the jobs of the latest attempt of a run.
func (c *Client) RunJobs(ctx context.Context, repo string, run int64) ([]Job, error) {
	var page struct {
		Jobs []Job `json:"jobs"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, run), nil, &page)
	return page.Jobs, err
}

// GetJob reads one job.
func (c *Client) GetJob(ctx context.Context, repo string, id int64) (Job, error) {
	var j Job
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/jobs/%d", repo, id), nil, &j)
	return j, err
}

// RunInfo is what a run says about itself.
type RunInfo struct {
	ID      int64  `json:"id"`
	Status  string `json:"status"`
	Event   string `json:"event"`
	Path    string `json:"path"` // the workflow file, e.g. ".github/workflows/ci.yml@refs/heads/main"
	HeadSHA string `json:"head_sha"`
}

// GetRun reads one run.
func (c *Client) GetRun(ctx context.Context, repo string, id int64) (RunInfo, error) {
	var r RunInfo
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d", repo, id), nil, &r)
	return r, err
}

// Workflow is a workflow file of a repository.
type Workflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// ListWorkflows lists the repository's workflows.
func (c *Client) ListWorkflows(ctx context.Context, repo string) ([]Workflow, error) {
	var page struct {
		Workflows []Workflow `json:"workflows"`
	}
	err := c.do(ctx, "GET", "/repos/"+repo+"/actions/workflows?per_page=100", nil, &page)
	return page.Workflows, err
}

// FileContent returns a file of the repository at a ref (a branch or sha; empty
// for the default branch).
func (c *Client) FileContent(ctx context.Context, repo, path, ref string) ([]byte, error) {
	p := "/repos/" + repo + "/contents/" + path
	if ref != "" {
		p += "?ref=" + ref
	}
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do(ctx, "GET", p, nil, &f); err != nil {
		return nil, err
	}
	if f.Encoding != "base64" {
		return nil, fmt.Errorf("github: %s has encoding %q", path, f.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

// JIT is a runner registered just-in-time: it takes one job and is removed.
type JIT struct {
	RunnerID int64
	// Config is what `run.sh --jitconfig` takes. It is a credential: keep it
	// out of logs and command lines.
	Config string
}

// OrgScope names a whole organization where a repository name is expected: its
// runners serve every repository of the organization.
const OrgScope = "org:"

// scopePath is the API path runners of a scope live under: /repos/o/r or /orgs/o.
func scopePath(scope string) string {
	if org, ok := strings.CutPrefix(scope, OrgScope); ok {
		return "/orgs/" + org
	}
	return "/repos/" + scope
}

// GenerateJIT registers a just-in-time runner for a repository, or for an
// organization (scope "org:name"), where it can take jobs of any repository
// that may use the runner group.
func (c *Client) GenerateJIT(ctx context.Context, repo, name string, labels []string) (JIT, error) {
	var res struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Config string `json:"encoded_jit_config"`
	}
	err := c.do(ctx, "POST", scopePath(repo)+"/actions/runners/generate-jitconfig",
		map[string]any{"name": name, "runner_group_id": 1, "labels": labels, "work_folder": "_work"}, &res)
	return JIT{RunnerID: res.Runner.ID, Config: res.Config}, err
}

// Runner is a registered runner.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online, offline
	Busy   bool   `json:"busy"`
}

// GetRunner reads a runner; NotFound once a just-in-time runner is gone.
func (c *Client) GetRunner(ctx context.Context, repo string, id int64) (Runner, error) {
	var r Runner
	err := c.do(ctx, "GET", fmt.Sprintf("%s/actions/runners/%d", scopePath(repo), id), nil, &r)
	return r, err
}

// DeleteRunner removes a runner (one that never took its job, say).
func (c *Client) DeleteRunner(ctx context.Context, repo string, id int64) error {
	err := c.do(ctx, "DELETE", fmt.Sprintf("%s/actions/runners/%d", scopePath(repo), id), nil, nil)
	if errors.Is(err, NotFound) {
		return nil
	}
	return err
}

// OrgRepos lists the full names of an organization's repositories that are not
// archived (the first 100).
func (c *Client) OrgRepos(ctx context.Context, org string) ([]string, error) {
	var repos []struct {
		FullName string `json:"full_name"`
		Archived bool   `json:"archived"`
	}
	if err := c.do(ctx, "GET", "/orgs/"+org+"/repos?per_page=100", nil, &repos); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if !r.Archived {
			out = append(out, r.FullName)
		}
	}
	return out, nil
}

// HasLabelPrefix reports whether the job asks for one of our labels.
func (j Job) HasLabelPrefix(prefix string) bool {
	for _, l := range j.Labels {
		if strings.HasPrefix(strings.ToLower(l), prefix) {
			return true
		}
	}
	return false
}
