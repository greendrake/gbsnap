package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/run"
)

// Init marks a directory as a place for pools, the only kind of directory a
// pool is ever created in on the receiving side of a send.
func (e *Engine) Init(ctx context.Context, loc location.Location, sudo run.SudoMode, force bool) error {
	r := e.runner(loc, sudo)
	id, err := pool.ReadPlace(ctx, r, loc.Path)
	if err != nil {
		return err
	}
	if id != "" {
		e.printer.Action("%s is a place for pools already, with ID %s", loc, id)
		return nil
	}
	if id, err = pool.NewPlaceID(); err != nil {
		return err
	}
	return e.do(fmt.Sprintf("mark %s as a place for pools, with ID %s", loc, id), func() error {
		return pool.MarkPlace(ctx, r, loc.Path, id, force)
	})
}

// Forget drops what a volume's pool records of a target retired for good, so
// that pruning stops keeping a snapshot for it. The target is named by its
// record's key or its place's ID, as status shows them, or by where it was
// last reached, as its pool or as the place holding it. Where that names
// several targets, as it does two disks that took turns at one mount point, it
// is refused, and only a key or an ID will do. It reports how many records
// went.
func (e *Engine) Forget(ctx context.Context, v *config.Volume, what string) (int, error) {
	s, err := e.open(ctx, v)
	if err != nil {
		return 0, err
	}
	rs, err := s.loadRecords(ctx)
	if err != nil {
		return 0, err
	}
	byKey := func(r pool.Record) bool {
		return r.Target == what || (pool.IsPlaceID(what) && strings.HasPrefix(r.Target, what+"/"))
	}
	named, parseErr := location.Parse(what)
	byLocation := func(r pool.Record) bool {
		at, err := location.Parse(r.Location)
		return err == nil && parseErr == nil && (at.Equal(named) || at.Parent().Equal(named))
	}
	match := byKey
	if !slices.ContainsFunc(rs.List, byKey) {
		match = byLocation
		var keys []string
		for _, r := range rs.List {
			if byLocation(r) {
				keys = append(keys, r.Target)
			}
		}
		if len(keys) > 1 {
			return 0, fmt.Errorf("%s was where more than one target was reached (%s); forget one by its key, "+
				"as status shows it", what, strings.Join(keys, ", "))
		}
	}
	dropped := rs.Drop(match)
	if len(dropped) == 0 {
		return 0, nil
	}
	var gone []string
	for _, r := range dropped {
		gone = append(gone, r.Location)
	}
	return len(dropped), e.do(fmt.Sprintf("%s: forget %s", v.Name, strings.Join(gone, ", ")), func() error {
		return s.src.SaveRecords(ctx, rs)
	})
}

// Untag takes the tag off a snapshot, in the pool and at every target that can
// be reached, after which pruning treats it like any other. A target that is
// offline has its copy renamed when it is next reached, by the reconciling
// every sync and pruning begins with.
func (e *Engine) Untag(ctx context.Context, v *config.Volume, name string) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	n, ok := s.src.Find(name)
	if !ok {
		return fmt.Errorf("pool %s holds no snapshot %s", v.Pool, name)
	}
	if !n.Protected() {
		return fmt.Errorf("%s carries no tag", n)
	}
	to := n.Untagged()
	if _, ok := s.src.Find(to.String()); ok {
		return fmt.Errorf("pool %s holds %s already", v.Pool, to)
	}
	if err := e.do(fmt.Sprintf("rename %s to %s", v.Pool.Child(n.String()), to), func() error {
		return s.src.Runner().Run(ctx, "mv", "-T", "--", s.src.Path(n), s.src.Path(to))
	}); err != nil {
		return err
	}
	s.src.Rename(n, to)

	rs, err := s.loadRecords(ctx)
	if err != nil {
		return err
	}
	if rs.Rename(n, to) && !e.dryRun {
		if err := s.src.SaveRecords(ctx, rs); err != nil {
			return err
		}
	}

	for _, loc := range v.Targets {
		t, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		if t.offline != "" {
			e.printer.Action("%s: %s is offline, so its copy is renamed when it is next reached: %s", v.Name, loc, t.offline)
			continue
		}
		if err := s.reconcileTags(ctx, t.pool); err != nil {
			return err
		}
	}
	return nil
}

// Subvolumes lists the subvolumes directly inside dir whose names do not start
// with a dot: what a pattern in the configuration stands for. A directory
// that is not there holds none.
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
	return names, nil
}

// Restore brings a volume's pool back from the first of its targets that can
// be reached and holds a pool for it: everything that pool holds past what the
// two share, or all of it when they share none, or, with a snapshot named,
// that one snapshot however old, from the first target that holds it. It is replication run the other way, the
// volume's own pool the destination.
func (e *Engine) Restore(ctx context.Context, v *config.Volume, snapshot string) error {
	if len(v.Targets) == 0 {
		return fmt.Errorf("%s has no targets to restore from", v.Name)
	}
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	var passed []string
	for _, loc := range v.Targets {
		t, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		switch {
		case t.offline != "":
			passed = append(passed, fmt.Sprintf("%s is offline: %s", loc, t.offline))
			continue
		case len(t.pool.Names()) == 0:
			passed = append(passed, fmt.Sprintf("%s holds no snapshots", loc))
			continue
		case snapshot != "":
			named, err := pool.ParseName(snapshot)
			if err != nil {
				return err
			}
			if _, ok := t.pool.Holds(named); !ok {
				passed = append(passed, fmt.Sprintf("%s holds no snapshot %s", loc, snapshot))
				continue
			}
		}
		back := &volumeRun{
			e:       e,
			v:       &config.Volume{Name: v.Name, Pool: loc, Targets: []location.Location{v.Pool}, Sudo: v.Sudo, AdHoc: true},
			src:     t.pool,
			targets: map[string]*target{},
		}
		return back.sync(ctx, SyncOpts{Reach: true, Snapshot: snapshot})
	}
	return fmt.Errorf("nothing to restore %s from: %s", v.Name, strings.Join(passed, "; "))
}
