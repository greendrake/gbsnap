// Package engine carries out the work on one volume: taking snapshots,
// replicating them and retiring the old ones.
package engine

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
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
	// key is what the pool's records know the target by: the target pool's ID,
	// empty until the pool has one.
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

// target reaches one of the volume's targets, once per run. A target is taken
// as written: a pool that is not there is one yet to be created, and what is
// or is not mounted where it goes is for the caller to see to. The one target
// gbsnap cannot use is one on a host ssh cannot reach, which is offline.
func (s *volumeRun) target(ctx context.Context, loc location.Location) (*target, error) {
	if t, ok := s.targets[loc.String()]; ok {
		return t, nil
	}
	t := &target{loc: loc}
	p := pool.New(loc, s.e.runner(loc, s.v.Sudo))
	if err := p.Load(ctx); err != nil {
		if !run.Unreachable(err) {
			return nil, err
		}
		// ssh fails the same way whether the host is down or turns the login
		// away, so what ssh said goes along: a key that is refused must not
		// pass for a host that is off.
		t.offline = fmt.Sprintf("%s cannot be reached: %v", loc.Host, err)
		s.targets[loc.String()] = t
		return t, nil
	}
	id, err := p.ID(ctx)
	if err != nil {
		return nil, err
	}
	t.pool, t.key = p, id
	s.targets[loc.String()] = t
	return t, nil
}

// identify gives a reached target's pool an ID, if it is there and has none
// yet, so that the records can know it. A dry run gives none, as it changes
// nothing.
//
// A pool gbsnap 0.2.0 sent to has no ID, and its record a key of 0.2.0's
// shape: that record is carried over to the ID, so that it neither lingers nor
// pins a snapshot for a target that is no more. Only where it is plainly this
// pool's: the one such record for the pool's location, naming a snapshot the
// pool holds. A pool made at that location while the one the record is about
// is away, a disk taking its turn elsewhere, holds no such snapshot, and the
// record stays to keep what that one needs.
func (s *volumeRun) identify(ctx context.Context, t *target) error {
	if t.key != "" || !t.pool.Exists() || s.e.dryRun {
		return nil
	}
	id, err := pool.NewID()
	if err != nil {
		return err
	}
	if err := t.pool.SetID(ctx, id); err != nil {
		return err
	}
	t.key = id
	if s.v.AdHoc {
		return nil
	}
	rs, err := s.loadRecords(ctx)
	if err != nil {
		return err
	}
	var earlier []int
	for i, r := range rs.List {
		if r.Location == t.loc.String() && !pool.IsID(r.Target) {
			earlier = append(earlier, i)
		}
	}
	if len(earlier) != 1 {
		return nil
	}
	named, err := pool.ParseName(rs.List[earlier[0]].Snapshot)
	if err != nil {
		return nil
	}
	if _, ok := t.pool.Holds(named); !ok {
		return nil
	}
	rs.List[earlier[0]].Target = id
	return s.src.SaveRecords(ctx, rs)
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
// nothing. Nor does a send between pools named outright, a restore among
// them: the records are what a configured volume's pool keeps for its targets,
// and the source of a send must never need to be written to.
func (s *volumeRun) remember(ctx context.Context, t *target) error {
	if s.v.AdHoc || t.key == "" {
		return nil
	}
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
	// Reach makes a target that cannot be reached a failure rather than
	// something to skip.
	Reach bool
	// Snapshot, when set, sends that one snapshot to each target lacking it,
	// however old, instead of everything newer than what the two share.
	Snapshot string
}

// Sync copies a volume's snapshots to each of its targets. A destination named
// outright on the command line has to be reached, as its source has to exist:
// neither is something to skip.
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
// it still needs. A target that cannot be reached is no failure unless reach
// is set, and pruning then keeps what its record says it needs.
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

// Subvolumes lists the subvolumes directly inside dir whose names do not start
// with a dot: what a pattern in the configuration stands for. A directory
// that is not there holds none, and a mount there is none of them.
func (e *Engine) Subvolumes(ctx context.Context, dir location.Location, sudo run.SudoMode) ([]string, error) {
	r := e.runner(dir, sudo)
	there, err := r.Test(ctx, "test", "-d", dir.Path)
	if err != nil || !there {
		return nil, err
	}
	// A subvolume's root directory is always inode 256, which is how one is
	// told from a plain directory without asking btrfs about each in turn.
	out, err := r.Output(ctx, "find", dir.Path, "-mindepth", "1", "-maxdepth", "1",
		"-type", "d", "-inum", "256", "!", "-name", ".*", "-printf", "%f\n")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		return names, nil
	}
	// A mount there is another filesystem, whose top directory is inode 256
	// as a subvolume's is. A snapshot can't leave its filesystem, so one of it
	// could never go into a pool beside the others, and backing it up is its
	// own owner's business: it is left out, saying so.
	mounts, err := mountsIn(ctx, r, dir.Path)
	if err != nil {
		return nil, err
	}
	kept := names[:0]
	for _, name := range names {
		if mounts[name] {
			e.printer.Action("%s is a mount of another filesystem, so it is left out", dir.Child(name))
			continue
		}
		kept = append(kept, name)
	}
	return kept, nil
}

// mountsIn names the mounts directly inside dir. They come from the mount
// table, not from device numbers, which every subvolume has of its own; the
// table holds each mount's path with its symbolic links resolved, so dir's
// are too, and findmnt's raw output escapes unsafe characters as \xNN.
func mountsIn(ctx context.Context, r run.Runner, dir string) (map[string]bool, error) {
	real, err := r.Output(ctx, "realpath", "-e", "--", dir)
	if err != nil {
		return nil, err
	}
	real = strings.TrimSuffix(real, "\n")
	out, err := r.Output(ctx, "findmnt", "-rn", "-o", "TARGET")
	if err != nil {
		return nil, err
	}
	mounts := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if target := unescapeHex(line); path.Dir(target) == real {
			mounts[path.Base(target)] = true
		}
	}
	return mounts, nil
}

// unescapeHex undoes findmnt's \xNN escapes.
func unescapeHex(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
