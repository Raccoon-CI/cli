package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Raccoon-CI/cli/internal/ghrunner"
)

// startWebhooks listens for GitHub's deliveries on listen and points a webhook
// of every repository at publicURL. The signing secret is made here, kept in
// memory and sent to GitHub only; it is never printed. The returned function
// stops the listener and deletes the webhooks.
func startWebhooks(ctx context.Context, gh *ghrunner.Client, s *ghrunner.Service, repos []string, listen, publicURL string, logf func(string, ...any)) (func(), error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(raw)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("webhook listener: %w", err)
	}
	srv := &http.Server{
		Handler:           &ghrunner.Webhook{Secret: []byte(secret), S: s},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("webhook listener: %v", err)
		}
	}()
	remove, err := ghrunner.RegisterHooks(ctx, gh, repos, publicURL, secret)
	if err != nil {
		srv.Close()
		return nil, err
	}
	logf("webhooks: %d repositories deliver workflow_job events to %s (listening on %s)", len(repos), publicURL, ln.Addr())
	return func() {
		bg, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		remove(bg)
		srv.Shutdown(bg)
	}, nil
}
