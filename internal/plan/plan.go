// Package plan works out, before anything runs, which job instances a
// workflow will produce and what each one needs. Values that depend on
// run-time contexts (github, needs, ...) are marked as deferred instead of
// failing, since they can only be resolved when the run starts.
package plan

import (
	"errors"
	"fmt"
	"io/fs"

	"raccoonci/internal/catalog"
	"raccoonci/internal/expr"
	"raccoonci/internal/platform"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/scan"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

type Plan struct {
	Jobs []*Job
}

type Job struct {
	*workflow.Job

	// DynamicMatrix is set when the matrix depends on run-time values, such
	// as `${{ fromJSON(needs.setup.outputs.matrix) }}`. Instances is then nil.
	DynamicMatrix bool
	Instances     []*Instance
}

// Instance is one run of a job: one matrix combination, or the job itself
// when it has no matrix.
type Instance struct {
	Matrix value.Object

	// RunsOn is the evaluated runs-on value, unless RunsOnDeferred.
	RunsOn         any
	RunsOnDeferred bool

	// OS is inferred from RunsOn; "" when the labels don't name one.
	OS string
	// UnavailableMsg is set when Raccoon CI can't provide OS: what to tell
	// the user.
	UnavailableMsg string

	// Needs is what must be installed before the instance runs. It is nil
	// for instances on an unavailable OS.
	Needs *scan.Result
}

// Options supply what scanning for needs uses. All are optional.
type Options struct {
	// Images says what GitHub's runner images preinstall.
	Images *runnerimages.Snapshot
	// Repo is the repository at the triggering commit. Use an os.OpenRoot
	// FS for untrusted repos.
	Repo fs.FS
	// Catalog defaults to catalog.Default().
	Catalog *catalog.Catalog
}

// Unavailable reports whether the instance needs an OS Raccoon CI can't
// provide. Per product decision such instances fail with a clear message
// (or go to a user-connected machine); they are never skipped or moved to
// another OS.
func (i *Instance) Unavailable() bool {
	return i.UnavailableMsg != ""
}

func Build(w *workflow.Workflow, opts Options) (*Plan, error) {
	if opts.Catalog == nil {
		opts.Catalog = catalog.Default()
	}
	p := &Plan{}
	for _, wj := range w.Jobs {
		j, err := buildJob(w, wj, opts)
		if err != nil {
			return nil, fmt.Errorf("job %q: %w", wj.ID, err)
		}
		p.Jobs = append(p.Jobs, j)
	}
	return p, nil
}

func buildJob(w *workflow.Workflow, wj *workflow.Job, opts Options) (*Job, error) {
	j := &Job{Job: wj}
	if wj.Uses != "" {
		return j, nil // the called workflow's jobs are planned when it is fetched
	}

	var rawMatrix any
	failFast, maxParallel := any(true), any(nil)
	if s := wj.Strategy; s != nil {
		rawMatrix = s.Matrix
		if s.FailFast != nil {
			failFast = s.FailFast
		}
		maxParallel = s.MaxParallel
	}
	m, deferred, err := evalEarly(rawMatrix, &expr.Context{})
	if err != nil {
		return nil, fmt.Errorf("matrix: %w", err)
	}
	if deferred {
		j.DynamicMatrix = true
		return j, nil
	}
	combos, err := workflow.ExpandMatrix(m)
	if err != nil {
		return nil, err
	}

	for idx, combo := range combos {
		mp := maxParallel
		if mp == nil {
			mp = float64(len(combos))
		}
		ctx := &expr.Context{Contexts: map[string]any{
			"matrix": combo,
			"strategy": value.Object{
				{Key: "fail-fast", Value: failFast},
				{Key: "job-index", Value: float64(idx)},
				{Key: "job-total", Value: float64(len(combos))},
				{Key: "max-parallel", Value: mp},
			},
		}}
		inst := &Instance{Matrix: combo}
		inst.RunsOn, inst.RunsOnDeferred, err = evalEarly(wj.RunsOn, ctx)
		if err != nil {
			return nil, fmt.Errorf("runs-on: %w", err)
		}
		if !inst.RunsOnDeferred {
			inst.OS = platform.FromRunsOn(opts.Catalog, inst.RunsOn)
			if inst.OS != "" && !opts.Catalog.OSAvailable(inst.OS) {
				inst.UnavailableMsg = opts.Catalog.OSUnavailable(inst.OS)
			}
		}
		if !inst.Unavailable() {
			inst.Needs = scan.Job(scan.Input{
				Workflow: w,
				Job:      wj,
				Context:  ctx,
				Image:    imageFor(opts.Catalog, inst, opts.Images),
				Repo:     opts.Repo,
				Catalog:  opts.Catalog,
			})
		}
		j.Instances = append(j.Instances, inst)
	}
	return j, nil
}

// imageFor returns the GitHub image the instance's runs-on names, if any.
func imageFor(cat *catalog.Catalog, inst *Instance, images *runnerimages.Snapshot) *runnerimages.Defaults {
	if images == nil || inst.RunsOnDeferred {
		return nil
	}
	for _, l := range platform.Labels(inst.RunsOn) {
		if d := images.ForLabel(cat, l); d != nil {
			return d
		}
	}
	return nil
}

// evalEarly evaluates v with the contexts known at plan time. deferred is
// true when v references a context that is only available at run time.
func evalEarly(v any, ctx *expr.Context) (result any, deferred bool, err error) {
	result, err = expr.EvalValue(v, ctx)
	var unknown *expr.UnknownContextError
	if errors.As(err, &unknown) {
		return nil, true, nil
	}
	return result, false, err
}
