package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
)

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
