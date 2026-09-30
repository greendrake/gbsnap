package engine

import (
	"context"
	"fmt"

	"github.com/greendrake/gbsnap/internal/config"
)

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
