package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/greendrake/gbsnap/internal/btrfs"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
)

func (s *volumeRun) sync(ctx context.Context, o SyncOpts) error {
	if len(s.v.Targets) == 0 {
		s.e.printer.Detail("%s: no targets, so nothing to send", s.v.Name)
		return nil
	}
	// A snapshot asked for by name has to be there, whatever else the pool
	// holds or does not.
	var want pool.Name
	if o.Snapshot != "" {
		named, err := pool.ParseName(o.Snapshot)
		if err != nil {
			return err
		}
		n, ok := s.src.Holds(named)
		if !ok {
			return fmt.Errorf("pool %s holds no snapshot %s", s.v.Pool, o.Snapshot)
		}
		want = n
	}
	if len(s.src.Names()) == 0 {
		s.e.printer.Detail("%s: pool %s holds no snapshots to send", s.v.Name, s.v.Pool)
		return nil
	}
	// One unreachable target must not keep the others from being brought up to
	// date, so every target is attempted and the failures reported together.
	var errs []error
	for _, loc := range s.v.Targets {
		if err := s.syncTo(ctx, loc, o, want); err != nil {
			errs = append(errs, fmt.Errorf("target %s: %w", loc, err))
		}
	}
	return errors.Join(errs...)
}

func (s *volumeRun) syncTo(ctx context.Context, loc location.Location, o SyncOpts, want pool.Name) error {
	t, err := s.target(ctx, loc)
	if err != nil {
		return err
	}
	if t.offline != "" {
		if o.Reach {
			return fmt.Errorf("offline: %s", t.offline)
		}
		s.e.printer.Action("%s: skipping %s, which is offline: %s", s.v.Name, loc, t.offline)
		return nil
	}
	if err := s.reconcileTags(ctx, t.pool); err != nil {
		return err
	}
	if err := s.ensure(ctx, t.pool); err != nil {
		return err
	}
	if o.Snapshot != "" {
		return s.fetch(ctx, t, want)
	}
	return s.catchUp(ctx, t)
}

// catchUp sends a target everything newer than what it already shares with
// the pool.
func (s *volumeRun) catchUp(ctx context.Context, t *target) error {
	dst := t.pool
	// The newest snapshot both pools hold is the one an incremental send can
	// build on, and everything after it is what the target is missing. Choosing
	// it by what is actually present means a target that fell behind, or that
	// had snapshots removed, recovers by itself.
	parent, hasParent := pool.Common(s.src.Names(), dst.Names())
	pending := pool.After(s.src.Names(), parent, hasParent)
	if len(pending) == 0 {
		s.e.printer.Detail("%s: %s is up to date", s.v.Name, t.loc)
		return s.remember(ctx, t)
	}
	if err := s.buildsOn(ctx, dst, parent, hasParent); err != nil {
		return err
	}
	for _, n := range pending {
		if err := s.transfer(ctx, t, parent, hasParent, n); err != nil {
			return err
		}
		parent, hasParent = n, true
	}
	return nil
}

// fetch sends one snapshot to a target lacking it, however old it is: what
// catchUp would never send, since it only looks past what the two share. It
// is how a snapshot the pool on the other side has already let go of comes
// back. It goes as a change from the shared snapshot nearest to it, where
// there is one.
func (s *volumeRun) fetch(ctx context.Context, t *target, want pool.Name) error {
	if _, ok := t.pool.Holds(want); ok {
		s.e.printer.Detail("%s: %s already holds %s", s.v.Name, t.loc, want)
		return s.remember(ctx, t)
	}
	parent, hasParent := nearestShared(s.src.Names(), t.pool.Names(), want)
	if err := s.buildsOn(ctx, t.pool, parent, hasParent); err != nil {
		return err
	}
	return s.transfer(ctx, t, parent, hasParent, want)
}

// nearestShared picks the snapshot both pools hold that is nearest to n: the
// newest one older than n, or failing that the oldest one newer. A send
// streams the difference between two snapshots whichever is the older.
func nearestShared(src, dst []pool.Name, n pool.Name) (pool.Name, bool) {
	held := map[string]bool{}
	for _, d := range dst {
		held[d.Key()] = true
	}
	var before, after pool.Name
	var hasBefore, hasAfter bool
	for _, e := range src {
		switch {
		case !held[e.Key()]:
		case e.Before(n):
			before, hasBefore = e, true
		case n.Before(e) && !hasAfter:
			after, hasAfter = e, true
		}
	}
	if hasBefore {
		return before, true
	}
	return after, hasAfter
}

// buildsOn checks that the target's copy of the parent a send is about to
// build on really is a copy of the pool's. Sending a difference against a
// parent the target did not get from this pool would produce a stream it
// cannot apply, or worse, one it applies to the wrong history.
func (s *volumeRun) buildsOn(ctx context.Context, dst *pool.Pool, parent pool.Name, hasParent bool) error {
	if !hasParent {
		return nil
	}
	theirs, _ := dst.Holds(parent)
	if err := s.isCopyOfOurs(ctx, dst, parent, theirs); err != nil {
		return fmt.Errorf("%s has diverged and cannot be built on: %w; "+
			"delete that snapshot at the target to send afresh, "+
			"or point the target at an empty pool", dst.Loc, err)
	}
	return nil
}

// reconcileTags takes a tag off the target's copy of a snapshot that has lost
// it in the pool since the two last met. A snapshot is paired with its copy by
// name, so a copy left under its old name would never be recognised as shared
// again, nor ever pruned. A tag only ever comes off (untag), so only a copy
// still carrying one is renamed: sent the other way, as a restore is, the pool
// is the backup, whose names are the stale ones, and nothing is renamed. A
// copy is renamed only once the UUID check has confirmed it is one.
func (s *volumeRun) reconcileTags(ctx context.Context, dst *pool.Pool) error {
	for _, theirs := range slices.Clone(dst.Names()) {
		if !theirs.Protected() {
			continue
		}
		ours, ok := s.src.Holds(theirs)
		if !ok || ours.Protected() {
			continue
		}
		if err := s.isCopyOfOurs(ctx, dst, ours, theirs); err != nil {
			return fmt.Errorf("%s is not a copy of %s, so it keeps its name: %w",
				dst.Loc.Child(theirs.String()), ours, err)
		}
		if err := s.e.do(fmt.Sprintf("rename %s to %s", dst.Loc.Child(theirs.String()), ours), func() error {
			return dst.Runner().Run(ctx, "mv", "-T", "--",
				path.Join(dst.Loc.Path, theirs.String()), path.Join(dst.Loc.Path, ours.String()))
		}); err != nil {
			return err
		}
		dst.Rename(theirs, ours)
	}
	return nil
}

// isCopyOfOurs checks that the target's snapshot theirs is a copy of the
// pool's snapshot ours, by the UUID trail btrfs receive leaves behind.
func (s *volumeRun) isCopyOfOurs(ctx context.Context, dst *pool.Pool, ours, theirs pool.Name) error {
	here, err := btrfs.Show(ctx, s.src.Runner(), s.src.Path(ours))
	if err != nil {
		return err
	}
	there, err := btrfs.Show(ctx, dst.Runner(), dst.Path(theirs))
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
	// Sent back the other way, as a restore is, the target holds the very
	// snapshot the pool's copy was received from.
	if here.ReceivedUUID != "" && there.UUID == here.ReceivedUUID {
		return nil
	}
	return fmt.Errorf("%s carries UUID %s and received UUID %q, and %s carries UUID %s and received UUID %q: "+
		"neither was received from the other, nor both from one source",
		dst.Loc.Child(theirs.String()), there.UUID, there.ReceivedUUID,
		s.src.Loc.Child(ours.String()), here.UUID, here.ReceivedUUID)
}

func (s *volumeRun) transfer(ctx context.Context, t *target, parent pool.Name, hasParent bool, n pool.Name) error {
	dst := t.pool
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
		if err := s.isCopyOfOurs(ctx, dst, n, n); err != nil {
			return fmt.Errorf("%s did not arrive intact: %w", n, err)
		}
		return nil
	}); err != nil {
		return err
	}
	dst.Add(n)
	return s.remember(ctx, t)
}
