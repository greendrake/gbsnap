// Package engine carries out the work on one volume: taking snapshots,
// replicating them and retiring the old ones.
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/run"
	"github.com/greendrake/gbsnap/internal/ui"
)

// Engine performs volume operations. Under dryRun it reads and verifies
// everything it normally would, reports the changes it would make, and makes
// none of them.
type Engine struct {
	printer *ui.Printer
	dryRun  bool
	now     func() time.Time
	// newRunner builds the runner for a host, replaced in tests.
	newRunner func(host string, sudo run.SudoMode) run.Runner
	runners   map[runnerKey]run.Runner
}

type runnerKey struct {
	host string
	sudo run.SudoMode
}

// New builds an engine.
func New(printer *ui.Printer, dryRun bool) *Engine {
	e := &Engine{
		printer: printer,
		dryRun:  dryRun,
		now:     time.Now,
		runners: map[runnerKey]run.Runner{},
	}
	e.newRunner = func(host string, sudo run.SudoMode) run.Runner {
		return run.NewExec(host, sudo, printer)
	}
	return e
}

// runner is the shared runner for a host, so that a host is asked at most once
// whether its commands need sudo.
func (e *Engine) runner(loc location.Location, sudo run.SudoMode) run.Runner {
	k := runnerKey{loc.Host, sudo}
	if r, ok := e.runners[k]; ok {
		return r
	}
	r := e.newRunner(loc.Host, sudo)
	e.runners[k] = r
	return r
}

// do reports a change and, unless this is a dry run, makes it.
func (e *Engine) do(desc string, change func() error) error {
	if e.dryRun {
		e.printer.Action("would %s", desc)
		return nil
	}
	e.printer.Action("%s", desc)
	return change()
}

// volumeRun holds the pools of one volume for the length of one operation, so
// that each is listed once however many phases look at it, and so that a dry
// run can reason about the pools its earlier phases would have left behind.
type volumeRun struct {
	e       *Engine
	v       *config.Volume
	src     *pool.Pool
	targets map[string]*pool.Pool
}

func (e *Engine) open(ctx context.Context, v *config.Volume) (*volumeRun, error) {
	src := pool.New(v.Pool, e.runner(v.Pool, v.Sudo))
	if err := src.Load(ctx); err != nil {
		return nil, err
	}
	if v.AdHoc && !src.Exists() {
		return nil, fmt.Errorf("pool %s does not exist", v.Pool)
	}
	return &volumeRun{e: e, v: v, src: src, targets: map[string]*pool.Pool{}}, nil
}

func (s *volumeRun) target(ctx context.Context, loc location.Location) (*pool.Pool, error) {
	if p, ok := s.targets[loc.String()]; ok {
		return p, nil
	}
	p := pool.New(loc, s.e.runner(loc, s.v.Sudo))
	if err := p.Load(ctx); err != nil {
		return nil, err
	}
	s.targets[loc.String()] = p
	return p, nil
}

// ensure creates a pool directory that is not there yet.
func (s *volumeRun) ensure(ctx context.Context, p *pool.Pool) error {
	if p.Exists() {
		return nil
	}
	return s.e.do("create pool "+p.Loc.String(), func() error { return p.Create(ctx) })
}

// Snap takes a snapshot of a volume's subvolume unless it is unchanged.
func (e *Engine) Snap(ctx context.Context, v *config.Volume, tag string, force bool) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	return s.snap(ctx, tag, force)
}

// Sync copies a volume's snapshots to each of its targets.
func (e *Engine) Sync(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	return s.sync(ctx)
}

// Prune retires the snapshots that a volume's retention no longer covers.
func (e *Engine) Prune(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	return s.prune(ctx)
}

// Run is the whole cycle for one volume. Pruning is skipped when replication
// failed: with a target's contents unknown, there is no telling which snapshots
// it still needs.
func (e *Engine) Run(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	if err := s.snap(ctx, "", false); err != nil {
		return err
	}
	if err := s.sync(ctx); err != nil {
		return err
	}
	return s.prune(ctx)
}
