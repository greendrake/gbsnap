package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/greendrake/gbsnap/internal/btrfs"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
)

func (s *volumeRun) sync(ctx context.Context) error {
	if len(s.v.Targets) == 0 {
		s.e.printer.Detail("%s: no targets, so nothing to send", s.v.Name)
		return nil
	}
	if len(s.src.Names()) == 0 {
		s.e.printer.Detail("%s: pool %s holds no snapshots to send", s.v.Name, s.v.Pool)
		return nil
	}
	// One unreachable target must not keep the others from being brought up to
	// date, so every target is attempted and the failures reported together.
	var errs []error
	for _, t := range s.v.Targets {
		if err := s.syncTo(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("target %s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

func (s *volumeRun) syncTo(ctx context.Context, loc location.Location) error {
	dst, err := s.target(ctx, loc)
	if err != nil {
		return err
	}
	if err := s.ensure(ctx, dst); err != nil {
		return err
	}

	// The newest snapshot both pools hold is the one an incremental send can
	// build on, and everything after it is what the target is missing. Choosing
	// it by what is actually present means a target that fell behind, or that
	// had snapshots removed, recovers by itself.
	parent, hasParent := pool.Common(s.src.Names(), dst.Names())
	pending := pool.After(s.src.Names(), parent, hasParent)
	if len(pending) == 0 {
		s.e.printer.Detail("%s: %s is up to date", s.v.Name, loc)
		return nil
	}
	// Sending a difference against a parent the target did not get from this
	// pool would produce a stream it cannot apply, or worse, one it applies to
	// the wrong history.
	if hasParent {
		if err := s.isCopyOfOurs(ctx, dst, parent); err != nil {
			return fmt.Errorf("%s has diverged and cannot be built on: %w; "+
				"delete that snapshot at the target to send afresh, "+
				"or point the target at an empty pool", dst.Loc, err)
		}
	}

	for _, n := range pending {
		if err := s.transfer(ctx, dst, parent, hasParent, n); err != nil {
			return err
		}
		parent, hasParent = n, true
	}
	return nil
}

// isCopyOfOurs checks that the target's copy of a snapshot is one this pool
// sent it, by the UUID trail btrfs receive leaves behind.
func (s *volumeRun) isCopyOfOurs(ctx context.Context, dst *pool.Pool, n pool.Name) error {
	here, err := btrfs.Show(ctx, s.src.Runner(), s.src.Path(n))
	if err != nil {
		return err
	}
	there, err := btrfs.Show(ctx, dst.Runner(), dst.Path(n))
	if err != nil {
		return err
	}
	// A received snapshot carries the UUID of the stream it came from: the
	// sender's own UUID, or the UUID the sender itself received when the
	// snapshot is being passed along a chain of pools.
	if there.ReceivedUUID != "" &&
		(there.ReceivedUUID == here.UUID || there.ReceivedUUID == here.ReceivedUUID) {
		return nil
	}
	return fmt.Errorf("%s carries received UUID %q, which is neither the UUID (%s) "+
		"nor the received UUID (%q) of %s",
		dst.Loc.Child(n.String()), there.ReceivedUUID, here.UUID, here.ReceivedUUID,
		s.src.Loc.Child(n.String()))
}

func (s *volumeRun) transfer(ctx context.Context, dst *pool.Pool, parent pool.Name, hasParent bool, n pool.Name) error {
	opts := btrfs.SendOpts{}
	desc := fmt.Sprintf("send %s to %s in full", s.src.Loc.Child(n.String()), dst.Loc)
	if hasParent {
		opts.Parent = s.src.Path(parent)
		desc = fmt.Sprintf("send %s to %s as a change since %s", s.src.Loc.Child(n.String()), dst.Loc, parent)
	}
	if err := s.e.do(desc, func() error {
		_, err := s.src.Runner().Pipe(ctx,
			btrfs.SendArgv(opts, s.src.Path(n)), dst.Runner(), btrfs.ReceiveArgv(dst.Loc.Path))
		if err != nil {
			return err
		}
		// What arrived is checked before anything downstream relies on it, and
		// before pruning trusts it enough to let go of history.
		if err := s.isCopyOfOurs(ctx, dst, n); err != nil {
			return fmt.Errorf("%s did not arrive intact: %w", n, err)
		}
		return nil
	}); err != nil {
		return err
	}
	dst.Add(n)
	return nil
}
