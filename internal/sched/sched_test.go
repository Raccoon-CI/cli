package sched

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEstimateIsTheMedianOfTheLastFive(t *testing.T) {
	h, err := OpenHistory(filepath.Join(t.TempDir(), "h.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Estimate("a"); ok {
		t.Fatal("estimate without history")
	}
	for _, s := range []int{100, 1, 2, 3, 50, 4} { // 100 falls out of the last five
		h.Record("a", time.Duration(s)*time.Second)
	}
	// 1 2 3 50 4 -> sorted 1 2 3 4 50 -> 3
	if d, _ := h.Estimate("a"); d != 3*time.Second {
		t.Errorf("estimate = %v, want 3s", d)
	}
}

func TestMaxIsTheLargestOfTheLastFive(t *testing.T) {
	h, err := OpenHistory(filepath.Join(t.TempDir(), "h.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Max("a"); ok {
		t.Fatal("a maximum without history")
	}
	for _, s := range []int{100, 1, 2, 3, 50, 4} { // 100 falls out of the last five
		h.Record("a", time.Duration(s)*time.Second)
	}
	if d, _ := h.Max("a"); d != 50*time.Second {
		t.Errorf("max = %v, want 50s", d)
	}
}

func TestHistoryOutlivesTheProcessAndSurvivesADamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "h.json")
	h, _ := OpenHistory(path)
	if err := h.Record("a", 7*time.Second); err != nil {
		t.Fatal(err)
	}
	h2, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := h2.Estimate("a"); !ok || d != 7*time.Second {
		t.Errorf("reopened estimate = %v, %v", d, ok)
	}
	writeFile(t, path, "{not json")
	h3, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("a damaged file should read as empty: %v", err)
	}
	if _, ok := h3.Estimate("a"); ok {
		t.Error("estimate from a damaged file")
	}
}

func TestShortComparesWithTheMeasuredStartup(t *testing.T) {
	h, _ := OpenHistory(filepath.Join(t.TempDir(), "h.json"))
	h.Record("quick", 5*time.Second)
	h.Record("slow", 60*time.Second)
	startup := time.Duration(0)
	p := &Policy{History: h, Startup: func() time.Duration { return startup }}

	if s, _, _ := p.Short("unknown"); s {
		t.Error("a job with no history must count as long")
	}
	if s, _, _ := p.Short("quick"); !s {
		t.Error("5 s is below 3 x the default 10 s start-up")
	}
	if s, _, _ := p.Short("slow"); s {
		t.Error("60 s is not short")
	}
	startup = 25 * time.Second // slow machines: now 60 s is below 75 s
	if s, _, limit := p.Short("slow"); !s || limit != 75*time.Second {
		t.Errorf("slow = %v, limit %v", s, limit)
	}
	if s, _, _ := (*Policy)(nil).Short("quick"); s {
		t.Error("a nil policy packs nothing")
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
