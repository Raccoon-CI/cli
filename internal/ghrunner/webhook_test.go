package ghrunner

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestSignatureIsChecked(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := sign("s3cret", body)
	for name, c := range map[string]struct {
		secret, header string
		want           bool
	}{
		"good":           {"s3cret", good, true},
		"wrong secret":   {"other", good, false},
		"no header":      {"s3cret", "", false},
		"no prefix":      {"s3cret", strings.TrimPrefix(good, "sha256="), false},
		"not hex":        {"s3cret", "sha256=zz", false},
		"sha1 is no use": {"s3cret", "sha1=" + strings.TrimPrefix(good, "sha256="), false},
	} {
		if got := validSignature([]byte(c.secret), body, c.header); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func deliver(t *testing.T, url, secret, event, payload string) int {
	t.Helper()
	body := []byte(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// A job that GitHub says is queued gets its runner without any listing of the
// repository (the listing is set to once an hour), and a delivery that is not
// signed does nothing.
func TestQueuedJobFromAWebhookStartsARunnerWithoutPolling(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}} // no run lists a job: only the webhook knows it
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.Webhooks, s.SafetyPoll = true, time.Hour
	hook := httptest.NewServer(&Webhook{Secret: []byte("s3cret"), S: s})
	defer hook.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	queued := fmt.Sprintf(`{"action":"queued","workflow_job":{"id":71,"run_id":9,"name":"build","status":"queued","labels":["self-hosted","raccoon-4vcpu"],"created_at":%q},"repository":{"full_name":"o/r"}}`,
		time.Now().UTC().Format(time.RFC3339))
	deadline := time.Now().Add(5 * time.Second)
	for { // the service takes deliveries once it runs
		if code := deliver(t, hook.URL, "wrong", "workflow_job", queued); code != http.StatusUnauthorized {
			t.Fatalf("unsigned-by-us delivery got %d, want 401", code)
		}
		s.mu.Lock()
		up := s.runCtx != nil
		s.mu.Unlock()
		if up || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := ib.creates.Load(); n != 0 {
		t.Fatalf("%d machines created for deliveries with a wrong signature", n)
	}
	if code := deliver(t, hook.URL, "s3cret", "ping", `{"zen":"x"}`); code != http.StatusNoContent {
		t.Errorf("ping got %d", code)
	}
	if code := deliver(t, hook.URL, "s3cret", "workflow_job", queued); code != http.StatusNoContent {
		t.Fatalf("queued job got %d", code)
	}
	for ib.creates.Load() < 1 && time.Now().Before(deadline.Add(10*time.Second)) {
		time.Sleep(20 * time.Millisecond)
	}
	if ib.creates.Load() != 1 {
		t.Fatalf("%d machines for the queued job, want 1", ib.creates.Load())
	}
	// the same delivery again (GitHub redelivers) must not start a second one
	deliver(t, hook.URL, "s3cret", "workflow_job", queued)
	time.Sleep(200 * time.Millisecond)
	if n := ib.creates.Load(); n != 1 {
		t.Errorf("a redelivery started another machine (%d)", n)
	}
	// a repository we do not watch is accepted and ignored
	other := strings.Replace(queued, "o/r", "x/y", 1)
	if code := deliver(t, hook.URL, "s3cret", "workflow_job", other); code != http.StatusAccepted {
		t.Errorf("foreign repository got %d, want 202", code)
	}
	if n := ib.creates.Load(); n != 1 {
		t.Errorf("a repository that is not watched got a machine (%d)", n)
	}
}

// Webhooks of a repository: one pointing at our address is updated, else one is
// made, and they are all removed by the function RegisterHooks returns.
func TestHooksAreRegisteredAndRemoved(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		bodies = append(bodies, b.String())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/a/hooks":
			fmt.Fprint(w, `[{"id":5,"config":{"url":"https://other"}},{"id":6,"config":{"url":"https://me/hook"}}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/b/hooks":
			fmt.Fprint(w, `[]`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/b/hooks":
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":9,"config":{"url":"https://me/hook"}}`)
		case r.Method == "PATCH" || r.Method == "DELETE":
			w.WriteHeader(map[string]int{"PATCH": 200, "DELETE": 204}[r.Method])
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	gh := &Client{Base: srv.URL, Token: "t"}
	remove, err := RegisterHooks(context.Background(), gh, []string{"o/a", "o/b"}, "https://me/hook", "SIGNING-SECRET")
	if err != nil {
		t.Fatal(err)
	}
	remove(context.Background())
	mu.Lock()
	text := strings.Join(calls, "\n")
	mu.Unlock()
	for _, want := range []string{"PATCH /repos/o/a/hooks/6", "POST /repos/o/b/hooks", "DELETE /repos/o/a/hooks/6", "DELETE /repos/o/b/hooks/9"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hooks/5") {
		t.Errorf("a hook that is not ours was touched:\n%s", text)
	}
	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(all, `"workflow_job"`) || !strings.Contains(all, "SIGNING-SECRET") {
		t.Errorf("the hook was not made for workflow_job events with the secret: %s", all)
	}
}
