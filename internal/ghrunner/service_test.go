package ghrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/instabox"
	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
)

// fakeGitHub has one repository with a run whose jobs are given; a job is
// taken as soon as a runner with its label has been registered and has been
// asked about twice (the first answer says busy).
type fakeGitHub struct {
	mu        sync.Mutex
	jobs      []Job
	runners   map[int64]int // runner id -> times asked
	nextID    int64
	jitBody   []byte
	deleted   []int64
	cancelled []int64 // runs asked to be cancelled

	stillQueued bool // a job is still queued when its runner has gone
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case r.Method == "POST" && strings.HasPrefix(p, "/repos/o/r/actions/runs/") && strings.HasSuffix(p, "/cancel"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(p, "/repos/o/r/actions/runs/"), "%d", &id)
		f.cancelled = append(f.cancelled, id)
		w.WriteHeader(202)
	case p == "/repos/o/r/actions/runs":
		runs := []Run{}
		if r.URL.Query().Get("status") == "queued" {
			runs = append(runs, Run{ID: 7, Status: "queued"})
		}
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	case p == "/repos/o/r/actions/runs/7/jobs":
		json.NewEncoder(w).Encode(map[string]any{"jobs": f.jobs})
	case strings.HasPrefix(p, "/repos/o/r/actions/jobs/"):
		for _, j := range f.jobs {
			if p == fmt.Sprintf("/repos/o/r/actions/jobs/%d", j.ID) {
				j.Status, j.Conclusion = "completed", "success"
				if f.stillQueued { // its runner took another job
					j.Status, j.Conclusion = "queued", ""
				}
				j.CreatedAt = time.Unix(100, 0)
				j.StartedAt, j.CompletedAt = time.Unix(105, 0), time.Unix(130, 0)
				json.NewEncoder(w).Encode(j)
			}
		}
	case p == "/repos/o/r/actions/runners/generate-jitconfig":
		f.jitBody, _ = io.ReadAll(r.Body)
		f.nextID++
		f.runners[f.nextID] = 0
		fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"SECRET-JIT-CONFIG"}`, f.nextID)
	case strings.HasPrefix(p, "/repos/o/r/actions/runners/"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(p, "/repos/o/r/actions/runners/"), "%d", &id)
		if r.Method == "DELETE" {
			f.deleted = append(f.deleted, id)
			delete(f.runners, id)
			w.WriteHeader(204)
			return
		}
		n, ok := f.runners[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		f.runners[id] = n + 1
		if n >= 1 { // the job it took is over: GitHub removes a JIT runner
			delete(f.runners, id)
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"status":"online","busy":true}`, id)
	default:
		w.WriteHeader(404)
	}
}

// fakeInstaBox handles what Provider.Acquire, the file upload, exec and delete need.
type fakeInstaBox struct {
	mu       sync.Mutex
	machines map[string]bool
	files    map[string]string // path -> body
	execs    []string
	requests []instabox.CreateRequest // what machines were asked for
	refuse   atomic.Int32             // creates still to be refused with 429
	refused  atomic.Int32
	creates  atomic.Int32
	deletes  atomic.Int32
}

func (f *fakeInstaBox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/machines":
		var req instabox.CreateRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.TemplateID != "ci" || req.SnapshotID != "" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"bad_request","message":"want template ci"}}`)
			return
		}
		if f.refuse.Load() > 0 { // the start limit
			f.refuse.Add(-1)
			f.refused.Add(1)
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"code":"rate_limited","message":"sandbox start limit reached: 10 starts in the last minute","retryable":true}}`)
			return
		}
		f.creates.Add(1)
		f.requests = append(f.requests, req)
		f.machines["m_1"] = true
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_1","machineId":"m_1","state":"queued"}`)
	case r.Method == "GET" && r.URL.Path == "/v1/orgs":
		io.WriteString(w, `{"orgs":[{"allowedSpecs":[{"vcpus":4,"memMiB":8192},{"vcpus":2,"memMiB":4096},{"vcpus":8,"memMiB":16384}]}]}`)
	case strings.HasPrefix(r.URL.Path, "/v1/operations/"):
		io.WriteString(w, `{"id":"op","machineId":"m_1","state":"succeeded","machine":{"spec":{"vcpus":4,"memMiB":8192,"persistentDiskGiB":20,"rootDiskGiB":50}}}`)
	case r.Method == "PUT" && r.URL.Path == "/v1/machines/m_1/files":
		b, _ := io.ReadAll(r.Body)
		f.files[r.URL.Query().Get("path")] = string(b)
		w.WriteHeader(201)
	case r.Method == "POST" && r.URL.Path == "/v1/machines/m_1/exec":
		var b struct {
			Cmd string `json:"cmd"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		f.execs = append(f.execs, b.Cmd)
		io.WriteString(w, `{"exitCode":0,"stdout":"","stderr":""}`)
	case r.Method == "DELETE" && r.URL.Path == "/v1/machines/m_1":
		f.deletes.Add(1)
		delete(f.machines, "m_1")
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_2","machineId":"m_1","state":"queued"}`)
	default:
		w.WriteHeader(404)
	}
}

// With Idle set a runner waits before any job exists, a job that arrives is
// left to it (no second machine), and when it has had its job another is
// started to wait.
func TestIdleRunnerIsKeptWaitingAndRefilled(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 21, RunID: 7, Name: "first", Status: "queued", Labels: []string{"raccoon-4vcpu"}},
	}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	p := &instabox.Provider{
		Client:     &instabox.Client{URL: ibSrv.URL, Key: "k", Poll: time.Millisecond},
		Controller: &dispatch.Controller{},
		Template:   "ci",
	}
	s := &Service{GH: &Client{Base: ghSrv.URL, Token: "t"}, P: p, Repos: []string{"o/r"}, Poll: 10 * time.Millisecond, Idle: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(20 * time.Second)
	for ib.creates.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	// One machine was started although no job existed yet; the queued job was
	// left to it, so only the refill made a second one.
	if n := ib.creates.Load(); n < 2 || n > 3 {
		t.Errorf("%d machines created: want the waiting one and its refill", n)
	}
	gh.mu.Lock()
	var jit map[string]any
	json.Unmarshal(gh.jitBody, &jit)
	gh.mu.Unlock()
	if labels, _ := json.Marshal(jit["labels"]); string(labels) != `["self-hosted","raccoon-4vcpu"]` {
		t.Errorf("waiting runner's labels %s", labels)
	}
}

func TestServiceRunsAQueuedJobOnAFreshMachine(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 11, RunID: 7, Name: "build", Status: "queued", Labels: []string{"raccoon-4vcpu"}},
		{ID: 12, RunID: 7, Name: "elsewhere", Status: "queued", Labels: []string{"ubuntu-latest"}},
		{ID: 13, RunID: 7, Name: "done", Status: "completed", Labels: []string{"raccoon-4vcpu"}},
	}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()

	p := &instabox.Provider{
		Client:     &instabox.Client{URL: ibSrv.URL, Key: "k", Poll: time.Millisecond},
		Controller: &dispatch.Controller{},
		Template:   "ci",
	}
	var mu sync.Mutex
	var log []string
	s := &Service{
		GH:    &Client{Base: ghSrv.URL, Token: "t"},
		P:     p,
		Repos: []string{"o/r"},
		Poll:  10 * time.Millisecond,
		Logf: func(f string, a ...any) {
			mu.Lock()
			log = append(log, fmt.Sprintf(f, a...))
			mu.Unlock()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(20 * time.Second)
	for ib.deletes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Let a few more polls pass: the job must not be served twice.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if n := ib.creates.Load(); n != 1 {
		t.Errorf("%d machines created, want 1 (only the queued job with our label)", n)
	}
	if n := ib.deletes.Load(); n != 1 {
		t.Errorf("%d machines deleted, want 1", n)
	}
	gh.mu.Lock()
	var jit map[string]any
	json.Unmarshal(gh.jitBody, &jit)
	gh.mu.Unlock()
	labels, _ := json.Marshal(jit["labels"])
	if string(labels) != `["self-hosted","raccoon-4vcpu"]` {
		t.Errorf("runner labels %s", labels)
	}
	// The config reached the machine as a file, never on a command line or in the log.
	if ib.files["/data/home/.raccoonci-jit"] != "SECRET-JIT-CONFIG" {
		t.Errorf("files: %v", ib.files)
	}
	for _, e := range ib.execs {
		if strings.Contains(e, "SECRET") {
			t.Errorf("the runner's config is in a command: %s", e)
		}
	}
	mu.Lock()
	text := strings.Join(log, "\n")
	mu.Unlock()
	if strings.Contains(text, "SECRET") {
		t.Errorf("the runner's config is in the log:\n%s", text)
	}
	if !strings.Contains(text, "job 11 (build): done") || !strings.Contains(text, "queued→started 5.0s, ran 25.0s") {
		t.Errorf("log:\n%s", text)
	}
	// Whatever the runner left registered is removed.
	if len(gh.deleted) == 0 {
		t.Error("the runner was not removed")
	}
}
