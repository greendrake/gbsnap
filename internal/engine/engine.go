// Package engine carries out the work on one volume: taking snapshots,
// replicating them and retiring the old ones.
package engine

import (
	"context"
	"fmt"
	"io"
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

// Close lets go of the runners, ending any sudo keep-alive they started.
func (e *Engine) Close() {
	for _, r := range e.runners {
		if c, ok := r.(io.Closer); ok {
			c.Close()
		}
	}
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
	targets map[string]*target
	records *pool.Records // what the pool knows of its targets, read on first use
}

// target is one of a volume's targets, as this run found it.
type target struct {
	loc  location.Location
	pool *pool.Pool // nil while the target is offline
	// key is what the pool's records know the target by: the ID of the place
	// holding it and the pool's name there, or the pool's location for a pool
	// in a place gbsnap init never marked.
	key string
	// offline says why the target could not be reached, and is empty when it
	// was.
	offline string
}

func (e *Engine) open(ctx context.Context, v *config.Volume) (*volumeRun, error) {
	src := pool.New(v.Pool, e.runner(v.Pool, v.Sudo))
	if err := src.Load(ctx); err != nil {
		return nil, err
	}
	if v.AdHoc && !src.Exists() {
		return nil, fmt.Errorf("pool %s does not exist", v.Pool)
	}
	return &volumeRun{e: e, v: v, src: src, targets: map[string]*target{}}, nil
}

// target reaches one of the volume's targets, once per run. A target that is
// not there is offline rather than empty: a pool that does not exist is only
// taken for one yet to be created when it sits in a place gbsnap init marked,
// and a host ssh cannot reach is offline too.
func (s *volumeRun) target(ctx context.Context, loc location.Location) (*target, error) {
	if t, ok := s.targets[loc.String()]; ok {
		return t, nil
	}
	t := &target{loc: loc}
	unreachable := func(err error) (*target, error) {
		if !run.Unreachable(err) {
			return nil, err
		}
		t.offline = fmt.Sprintf("%s cannot be reached", loc.Host)
		s.targets[loc.String()] = t
		return t, nil
	}
	r := s.e.runner(loc, s.v.Sudo)
	place := loc.Parent()
	id, err := pool.ReadPlace(ctx, r, place.Path)
	if err != nil {
		return unreachable(err)
	}
	p := pool.New(loc, r)
	if err := p.Load(ctx); err != nil {
		return unreachable(err)
	}
	switch {
	case id != "":
		t.key = id + "/" + loc.Base()
		t.pool = p
	case p.Exists():
		t.key = loc.Canonical()
		t.pool = p
	default:
		t.offline = fmt.Sprintf("%s holds no pool %s and is not a place gbsnap init marked (is a disk not mounted?)",
			place, loc.Base())
	}
	s.targets[loc.String()] = t
	return t, nil
}

// loadRecords reads what the pool knows of its targets, once per run.
func (s *volumeRun) loadRecords(ctx context.Context) (*pool.Records, error) {
	if s.records != nil {
		return s.records, nil
	}
	rs, err := s.src.LoadRecords(ctx)
	if err != nil {
		return nil, err
	}
	s.records = rs
	return rs, nil
}

// remember records the newest snapshot a reachable target shares with the
// pool, or that it shares none. A dry run records nothing, as it changes
// nothing.
func (s *volumeRun) remember(ctx context.Context, t *target) error {
	rs, err := s.loadRecords(ctx)
	if err != nil {
		return err
	}
	var changed bool
	if n, ok := pool.Common(s.src.Names(), t.pool.Names()); ok {
		changed = rs.Set(pool.Record{Target: t.key, Location: t.loc.String(), Snapshot: n.String(), Since: s.e.now().UTC()})
	} else {
		changed = len(rs.Drop(func(r pool.Record) bool { return r.Target == t.key })) > 0
	}
	if !changed || s.e.dryRun {
		return nil
	}
	return s.src.SaveRecords(ctx, rs)
}

// recorded finds the record of a target, by its key when the run reached it
// and by where it was last reached when it did not.
func (s *volumeRun) recorded(ctx context.Context, t *target) (pool.Record, bool, error) {
	rs, err := s.loadRecords(ctx)
	if err != nil {
		return pool.Record{}, false, err
	}
	for _, r := range rs.List {
		if (t.key != "" && r.Target == t.key) || (t.key == "" && r.Location == t.loc.String()) {
			return r, true, nil
		}
	}
	return pool.Record{}, false, nil
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

// SyncOpts shapes a replication.
type SyncOpts struct {
	// Reach makes a target that is offline a failure rather than something to
	// skip.
	Reach bool
	// Snapshot, when set, sends that one snapshot to each target lacking it,
	// however old, instead of everything newer than what the two share.
	Snapshot string
}

// Sync copies a volume's snapshots to each of its targets. A destination named
// outright on the command line has to be reached, as its source has to exist:
// neither is something to skip in silence.
func (e *Engine) Sync(ctx context.Context, v *config.Volume, o SyncOpts) error {
	if v.AdHoc {
		o.Reach = true
	}
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	return s.sync(ctx, o)
}

// PruneOpts shapes a pruning.
type PruneOpts struct {
	// Local keeps pruning to the volume's own pool, reaching no target: what
	// each target needs is taken from the pool's records.
	Local bool
}

// Prune retires the snapshots that a volume's retention no longer covers.
func (e *Engine) Prune(ctx context.Context, v *config.Volume, o PruneOpts) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	return s.prune(ctx, o)
}

// Run is the whole cycle for one volume. Pruning is skipped when replication
// failed: with a target's contents unknown, there is no telling which snapshots
// it still needs. A target that is offline is no failure unless reach is set,
// and pruning then keeps what its record says it needs.
func (e *Engine) Run(ctx context.Context, v *config.Volume, reach bool) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	if err := s.snap(ctx, "", false); err != nil {
		return err
	}
	if err := s.sync(ctx, SyncOpts{Reach: reach}); err != nil {
		return err
	}
	return s.prune(ctx, PruneOpts{})
}
