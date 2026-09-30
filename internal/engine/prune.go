package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/greendrake/gbsnap/internal/btrfs"
	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/retention"
)

func (s *volumeRun) prune(ctx context.Context, o PruneOpts) error {
	// What each target still shares with the pool has to be worked out before
	// anything is deleted, or the first deletion would change the answer for
	// everything after it.
	var reached []*target
	shared := map[*target]pool.Name{}
	if !o.Local {
		for _, loc := range s.v.Targets {
			t, err := s.target(ctx, loc)
			if err != nil {
				return err
			}
			if t.offline != "" {
				s.e.printer.Detail("%s: %s: %s, so its record says what it needs", s.v.Name, loc, t.offline)
				continue
			}
			if err := s.reconcileTags(ctx, t.pool); err != nil {
				return err
			}
			if err := s.identify(ctx, t); err != nil {
				return err
			}
			if err := s.remember(ctx, t); err != nil {
				return err
			}
			if n, ok := pool.Common(s.src.Names(), t.pool.Names()); ok {
				shared[t] = n
			}
			reached = append(reached, t)
		}
	}

	// Every record pins its snapshot in the pool. Those of the targets just
	// reached say what they hold now; for any other — out of reach, left alone
	// by a local pruning, a disk taking its turn elsewhere, or no longer
	// configured at all until gbsnap forget lets go of it — the record is all
	// there is to go on.
	rs, err := s.loadRecords(ctx)
	if err != nil {
		return err
	}
	poolPins := map[string]bool{}
	for _, r := range rs.List {
		if n, ok := s.recordedSnapshot(r); ok {
			poolPins[n.String()] = true
		}
	}
	if err := s.prunePool(ctx, s.src, s.v.Retention.Pool, poolPins); err != nil {
		return err
	}
	for _, t := range reached {
		pins := map[string]bool{}
		if n, ok := shared[t]; ok {
			if theirs, ok := t.pool.Holds(n); ok {
				pins[theirs.String()] = true
			}
		}
		if err := s.prunePool(ctx, t.pool, s.v.Retention.Target, pins); err != nil {
			return err
		}
	}
	return nil
}

// recordedSnapshot finds the pool's snapshot a record names, under whatever
// tag it carries now.
func (s *volumeRun) recordedSnapshot(r pool.Record) (pool.Name, bool) {
	want, err := pool.ParseName(r.Snapshot)
	if err != nil {
		return pool.Name{}, false
	}
	for _, n := range s.src.Names() {
		if n.Same(want) {
			return n, true
		}
	}
	return pool.Name{}, false
}

func (s *volumeRun) prunePool(ctx context.Context, p *pool.Pool, policy retention.Policy, pins map[string]bool) error {
	for _, n := range retention.Select(p.Names(), policy, pins) {
		if err := s.e.do("delete "+p.Loc.Child(n.String()).String(), func() error {
			return btrfs.Delete(ctx, p.Runner(), p.Path(n))
		}); err != nil {
			return err
		}
		p.Remove(n)
	}
	return nil
}

// Forget drops what a volume's pool records of a target retired for good, so
// that pruning stops keeping a snapshot for it. The target is named by its
// record's key, its pool's ID, as status shows it, or by where it was last
// reached, as its pool or as the directory holding it. Where that names
// several targets, as it does two disks that took turns at one mount point, it
// is refused, and only the key will do. It reports how many records went.
func (e *Engine) Forget(ctx context.Context, v *config.Volume, what string) (int, error) {
	s, err := e.open(ctx, v)
	if err != nil {
		return 0, err
	}
	rs, err := s.loadRecords(ctx)
	if err != nil {
		return 0, err
	}
	byKey := func(r pool.Record) bool { return r.Target == what }
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
