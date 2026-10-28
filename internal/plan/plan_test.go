package plan

import (
	"testing"

	"raccoonci/internal/runnerimages"
	"raccoonci/internal/workflow"
)

func build(t *testing.T, src string) *Plan {
	t.Helper()
	w, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(w, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildEvaluatesRunsOnPerCombination(t *testing.T) {
	p := build(t, `
on: push
jobs:
  test:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest, macos-14, windows-2022]
    steps: [{run: make test}]
`)
	insts := p.Jobs[0].Instances
	want := []struct {
		runsOn      string
		os          string
		unavailable bool
	}{
		{"ubuntu-latest", "linux", false},
		{"macos-14", "macos", true},
		{"windows-2022", "windows", true},
	}
	if len(insts) != len(want) {
		t.Fatalf("got %d instances, want %d", len(insts), len(want))
	}
	for i, w := range want {
		got := insts[i]
		if got.RunsOn != w.runsOn || got.OS != w.os || got.Unavailable() != w.unavailable {
			t.Errorf("instance %d: runs-on %v, os %q, unavailable %v; want %s, %q, %v",
				i, got.RunsOn, got.OS, got.Unavailable(), w.runsOn, w.os, w.unavailable)
		}
	}
}

func TestBuildDefersRunTimeValues(t *testing.T) {
	p := build(t, `
on: push
jobs:
  setup:
    runs-on: ${{ github.event.inputs.runner }}
    steps: [{run: echo}]
  build:
    needs: setup
    runs-on: [self-hosted, gpu]
    strategy:
      matrix: ${{ fromJSON(needs.setup.outputs.matrix) }}
    steps: [{run: make}]
  deploy:
    needs: build
    uses: ./.github/workflows/deploy.yml
`)
	setup, build, deploy := p.Jobs[0], p.Jobs[1], p.Jobs[2]
	if i := setup.Instances[0]; !i.RunsOnDeferred || i.Unavailable() {
		t.Errorf("setup: runs-on should be deferred, got %+v", i)
	}
	if !build.DynamicMatrix || build.Instances != nil {
		t.Errorf("build: matrix should be dynamic, got %+v", build)
	}
	if deploy.Instances != nil || deploy.Uses == "" {
		t.Errorf("deploy: reusable workflow call should have no instances, got %+v", deploy)
	}
}

func TestBuildStrategyContext(t *testing.T) {
	p := build(t, `
on: push
jobs:
  shard:
    runs-on: ${{ format('ubuntu-{0}', strategy.job-index) }}
    strategy:
      matrix:
        shard: [a, b]
    steps: [{run: make}]
`)
	if got := p.Jobs[0].Instances[1].RunsOn; got != "ubuntu-1" {
		t.Errorf("runs-on = %v, want ubuntu-1", got)
	}
}

func TestBuildScansNeedsPerInstance(t *testing.T) {
	w, err := workflow.Parse([]byte(`
on: push
jobs:
  test:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-22.04, ubuntu-latest, macos-latest]
    steps: [{run: npm test}]
`))
	if err != nil {
		t.Fatal(err)
	}
	images, err := runnerimages.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(w, Options{Images: images})
	if err != nil {
		t.Fatal(err)
	}
	insts := p.Jobs[0].Instances
	for i, label := range []string{"ubuntu-22.04", "ubuntu-latest"} {
		want := images.ForLabel(nil, label).Tools["node"]
		if n := insts[i].Needs; n == nil || len(n.Demands) != 1 || n.Demands[0].Version != want {
			t.Errorf("%s: needs %+v, want node %s from that image", label, n, want)
		}
	}
	if insts[2].Needs != nil {
		t.Errorf("macos instance is unavailable and shouldn't be scanned, got %+v", insts[2].Needs)
	}
}

func TestBuildReportsRealExpressionErrors(t *testing.T) {
	w, err := workflow.Parse([]byte(`
on: push
jobs:
  a:
    runs-on: ${{ matrix.os ==  }}
    steps: [{run: x}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(w, Options{}); err == nil {
		t.Fatal("a syntax error in runs-on was accepted")
	}
}
