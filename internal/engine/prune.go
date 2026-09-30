package engine

import (
	"context"

	"github.com/greendrake/gbsnap/internal/btrfs"
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
				s.e.printer.Detail("%s: %s is offline, so its record says what it needs: %s", s.v.Name, loc, t.offline)
				continue
			}
			if err := s.reconcileTags(ctx, t.pool); err != nil {
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
	// reached say what they hold now; for any other — offline, left alone by a
	// local pruning, or no longer configured at all until gbsnap forget lets go
	// of it — the record is all there is to go on.
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
