package engine

import (
	"context"
	"fmt"

	"github.com/greendrake/gbsnap/internal/btrfs"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/run"
)

func (s *volumeRun) snap(ctx context.Context, tag string, force bool) error {
	if s.v.Subvolume == nil {
		s.e.printer.Detail("%s: no subvolume, so nothing to snapshot", s.v.Name)
		return nil
	}
	if err := s.ensure(ctx, s.src); err != nil {
		return err
	}

	latest, has := s.src.Latest()
	if has && !force {
		changed, decided, err := s.changed(ctx, latest)
		if err != nil {
			return err
		}
		if decided && !changed {
			s.e.printer.Action("%s: unchanged since %s", s.v.Name, latest)
			return nil
		}
	}

	name := pool.NextName(s.e.now(), s.src.Names(), tag)
	// A clock that has fallen behind the pool would name the snapshot into the
	// past, where replication never looks: only what follows the newest common
	// snapshot is ever sent, so the snapshot would silently reach no target.
	if has && name.Before(latest) {
		return fmt.Errorf("the clock reads %s but the pool already holds %s; fix the clock before snapshotting", name, latest)
	}
	dst := s.v.Pool.Child(name.String())
	if err := s.e.do(fmt.Sprintf("snapshot %s to %s", s.v.Subvolume, dst), func() error {
		return btrfs.Snapshot(ctx, s.src.Runner(), s.v.Subvolume.Path, dst.Path)
	}); err != nil {
		return err
	}
	s.src.Add(name)
	return nil
}

// changed tells whether the subvolume differs from its latest snapshot.
//
// It answers from generation numbers where they settle the matter, which costs
// nothing and, more to the point, writes nothing: a pool that is being left
// alone stays untouched however often gbsnap looks at it. Where the generation
// has moved the answer is inconclusive, because reading a file is enough to
// move it, and only then is the difference examined in full.
//
// The second result is false when a dry run declined to look that closely.
func (s *volumeRun) changed(ctx context.Context, latest pool.Name) (changed, decided bool, err error) {
	r := s.src.Runner()
	subvolume := s.v.Subvolume.Path

	// Writes that have not been committed yet are invisible to the generation,
	// so ask the filesystem to commit before reading it.
	if err := btrfs.SyncFS(ctx, r, subvolume); err != nil {
		return false, false, err
	}
	live, err := btrfs.Show(ctx, r, subvolume)
	if err != nil {
		return false, false, err
	}
	snapshot, err := btrfs.Show(ctx, r, s.src.Path(latest))
	if err != nil {
		return false, false, err
	}

	if snapshot.ParentUUID != live.UUID {
		// The newest snapshot is of something else, so there is nothing here to
		// compare against and the subvolume needs a snapshot of its own.
		s.e.printer.Detail("%s: %s is not a snapshot of %s", s.v.Name, latest, subvolume)
		return true, true, nil
	}
	if snapshot.Generation == live.Generation {
		return false, true, nil
	}
	if s.e.dryRun {
		s.e.printer.Action("would examine %s for changes since %s", subvolume, latest)
		return false, false, nil
	}

	changed, err = s.probe(ctx, latest)
	return changed, true, err
}

// probe compares the subvolume against a snapshot in full, by taking a
// throwaway snapshot of the subvolume as it stands and describing the
// difference. Timestamps aside, any difference at all counts.
func (s *volumeRun) probe(ctx context.Context, latest pool.Name) (changed bool, err error) {
	r := s.src.Runner()
	probe := s.src.ProbePath()

	// A probe snapshot outlives its run only if that run was interrupted. The
	// name is fixed, so at most one can ever be waiting here.
	leftover, err := r.Test(ctx, "test", "-e", probe)
	if err != nil {
		return false, err
	}
	if leftover {
		s.e.printer.Detail("%s: clearing the probe snapshot an interrupted run left at %s", s.v.Name, probe)
		if err := btrfs.Delete(ctx, r, probe); err != nil {
			return false, err
		}
	}

	if err := btrfs.Snapshot(ctx, r, s.v.Subvolume.Path, probe); err != nil {
		return false, err
	}
	defer func() {
		if cleanup := btrfs.Delete(ctx, r, probe); cleanup != nil && err == nil {
			err = cleanup
		}
	}()

	// The difference is decoded here rather than on the subvolume's host, and
	// without sudo: it needs neither root nor a btrfs filesystem, and --no-data
	// keeps what crosses the link down to a description of the change.
	send := btrfs.SendArgv(btrfs.SendOpts{Parent: s.src.Path(latest), NoData: true}, probe)
	dump, err := r.Pipe(ctx, send, s.e.runner(location.Location{}, run.SudoNever), btrfs.DumpArgv())
	if err != nil {
		return false, err
	}
	return btrfs.DumpChanged(dump), nil
}
