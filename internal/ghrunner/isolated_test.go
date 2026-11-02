package ghrunner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsPublic(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"public", `{"private":false,"visibility":"public"}`, true},
		{"private", `{"private":true,"visibility":"private"}`, false},
		{"internal", `{"private":true,"visibility":"internal"}`, false},
		{"visibility only", `{"visibility":"internal"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r" {
					t.Errorf("path %s", r.URL.Path)
				}
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			got, err := (&Client{Base: srv.URL, Token: "t"}).IsPublic(context.Background(), "o/r")
			if err != nil || got != c.want {
				t.Fatalf("IsPublic = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

func TestIsPublicErrorIsNotPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := (&Client{Base: srv.URL, Token: "t"}).IsPublic(context.Background(), "o/r"); err == nil {
		t.Fatal("a failed lookup must be an error, not an answer")
	}
}
