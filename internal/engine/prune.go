package engine

import (
	"context"

	"github.com/greendrake/gbsnap/internal/btrfs"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/retention"
)

func (s *volumeRun) prune(ctx context.Context) error {
	// What each target still shares with the pool has to be worked out before
	// anything is deleted, or the first deletion would change the answer for
	// everything after it.
	shared := map[string]pool.Name{}
	poolPins := map[string]bool{}
	for _, loc := range s.v.Targets {
		dst, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		if n, ok := pool.Common(s.src.Names(), dst.Names()); ok {
			shared[loc.String()] = n
			poolPins[n.String()] = true
		}
	}

	if err := s.prunePool(ctx, s.src, s.v.Retention.Pool, poolPins); err != nil {
		return err
	}
	for _, loc := range s.v.Targets {
		dst, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		pins := map[string]bool{}
		if n, ok := shared[loc.String()]; ok {
			pins[n.String()] = true
		}
		if err := s.prunePool(ctx, dst, s.v.Retention.Target, pins); err != nil {
			return err
		}
	}
	return nil
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
