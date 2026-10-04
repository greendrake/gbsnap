package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/engine"
	"github.com/greendrake/gbsnap/internal/pool"
)

// command is one of gbsnap's verbs.
type command struct {
	name  string
	usage string
	flags func(*flag.FlagSet)
	run   func(ctx context.Context, a *app, args []string) error
}

func lookup(name string) *command {
	for _, c := range commands() {
		if c.name == name {
			return c
		}
	}
	return nil
}

// commands builds the table afresh so that each command owns the variables its
// flags write into.
func commands() []*command {
	var tag, snapshot string
	var force, reach, local bool
	var min int

	return []*command{{
		name:  "run",
		usage: "run [<volume>...] [-reach]",
		flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&reach, "reach", false, "fail on a target that cannot be reached, rather than skip it")
		},
		run: func(ctx context.Context, a *app, args []string) error {
			volumes, err := a.volumes(ctx, args)
			if err != nil {
				return err
			}
			return a.each(ctx, volumes, true, func(ctx context.Context, v *config.Volume) error {
				return a.engine.Run(ctx, v, reach)
			})
		},
	}, {
		name:  "snap",
		usage: "snap [<volume>...] [-tag <name>] [-force]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&tag, "tag", "", "tag the new snapshot, protecting it from pruning")
			fs.BoolVar(&force, "force", false, "snapshot even if the subvolume is unchanged")
		},
		run: func(ctx context.Context, a *app, args []string) error {
			if tag != "" {
				if err := pool.ValidateTag(tag); err != nil {
					return usageError{err}
				}
			}
			volumes, err := a.volumes(ctx, args)
			if err != nil {
				return err
			}
			return a.each(ctx, volumes, true, func(ctx context.Context, v *config.Volume) error {
				return a.engine.Snap(ctx, v, tag, force)
			})
		},
	}, {
		name:  "sync",
		usage: "sync [<volume>...] | sync <src-pool> <dst-pool> [-reach] [-snapshot <name>]",
		flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&reach, "reach", false, "fail on a target that cannot be reached, rather than skip it")
			fs.StringVar(&snapshot, "snapshot", "", "send that one snapshot to each target lacking it, however old")
		},
		run: func(ctx context.Context, a *app, args []string) error {
			locs, adHoc, err := a.pools(args)
			if err != nil {
				return err
			}
			var volumes []*config.Volume
			switch {
			case !adHoc:
				if volumes, err = a.volumes(ctx, args); err != nil {
					return err
				}
			case len(locs) != 2:
				return usagef("syncing pools takes a source and a destination, given %d", len(locs))
			case locs[0].Equal(locs[1]):
				return usagef("the source and the destination are the same pool")
			default:
				volumes = []*config.Volume{config.AdHoc(locs[0], locs[1:], config.DefaultMin, a.sudo)}
			}
			if snapshot != "" {
				if _, err := pool.ParseName(snapshot); err != nil {
					return usageError{err}
				}
			}
			o := engine.SyncOpts{Reach: reach, Snapshot: snapshot}
			return a.each(ctx, volumes, true, func(ctx context.Context, v *config.Volume) error {
				return a.engine.Sync(ctx, v, o)
			})
		},
	}, {
		name:  "prune",
		usage: "prune [<volume>...] [-local] | prune <pool> -min <n>",
		flags: func(fs *flag.FlagSet) {
			fs.IntVar(&min, "min", 0, "how many recent snapshots to keep")
			fs.BoolVar(&local, "local", false, "prune the volume's own pool alone, reaching no target")
		},
		run: func(ctx context.Context, a *app, args []string) error {
			locs, adHoc, err := a.pools(args)
			if err != nil {
				return err
			}
			var volumes []*config.Volume
			switch {
			case !adHoc:
				if min != 0 {
					return usagef("-min applies to a pool named on the command line, not to configured volumes, which carry their own retention")
				}
				if volumes, err = a.volumes(ctx, args); err != nil {
					return err
				}
			case len(locs) != 1:
				return usagef("prune takes one pool, given %d", len(locs))
			case min < 1:
				return usagef("pruning a pool needs -min, at least 1")
			default:
				volumes = []*config.Volume{config.AdHoc(locs[0], nil, min, a.sudo)}
			}
			o := engine.PruneOpts{Local: local}
			return a.each(ctx, volumes, true, func(ctx context.Context, v *config.Volume) error {
				return a.engine.Prune(ctx, v, o)
			})
		},
	}, {
		name:  "list",
		usage: "list [<volume>|<pool>]",
		run: func(ctx context.Context, a *app, args []string) error {
			locs, adHoc, err := a.pools(args)
			if err != nil {
				return err
			}
			if adHoc {
				if len(locs) != 1 {
					return usagef("list takes one pool, given %d", len(locs))
				}
				return a.engine.List(ctx, config.AdHoc(locs[0], nil, 1, a.sudo))
			}
			volumes, err := a.poolVolumes(ctx, args)
			if err != nil {
				return err
			}
			if len(volumes) != 1 {
				return usagef("list takes one volume or pool; %d selected", len(volumes))
			}
			return a.engine.List(ctx, volumes[0])
		},
	}, {
		// Transitional, and goes with the rest of the conversion code.
		name:  "convert",
		usage: "convert [<volume>...] | convert <pool>...",
		run: func(ctx context.Context, a *app, args []string) error {
			locs, adHoc, err := a.pools(args)
			if err != nil {
				return err
			}
			var volumes []*config.Volume
			if adHoc {
				for _, loc := range locs {
					volumes = append(volumes, config.AdHoc(loc, nil, config.DefaultMin, a.sudo))
				}
			} else if volumes, err = a.volumes(ctx, args); err != nil {
				return err
			}
			return a.each(ctx, volumes, true, a.engine.Convert)
		},
	}, {
		name:  "status",
		usage: "status [<volume>...]",
		run: func(ctx context.Context, a *app, args []string) error {
			volumes, err := a.poolVolumes(ctx, args)
			if err != nil {
				return err
			}
			// Status only reads, so it takes no locks: it has to answer while
			// a backup run is holding them.
			return a.each(ctx, volumes, false, a.engine.Status)
		},
	}, {
		name:  "restore",
		usage: "restore <volume> [-snapshot <name>]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&snapshot, "snapshot", "", "bring back that one snapshot, however old")
		},
		run: func(ctx context.Context, a *app, args []string) error {
			if len(args) != 1 {
				return usagef("restore takes one volume, given %d", len(args))
			}
			if snapshot != "" {
				if _, err := pool.ParseName(snapshot); err != nil {
					return usageError{err}
				}
			}
			cfg, err := a.config(ctx)
			if err != nil {
				return err
			}
			// A volume a pattern stands for can be restored before its
			// subvolume is there: bringing it back is the point.
			v, err := cfg.Volume(args[0])
			if err != nil {
				if v, err = cfg.Absent(args[0]); err != nil {
					return usageError{err}
				}
			}
			if a.sudoGiven {
				v.Sudo = a.sudo
			}
			return a.each(ctx, []*config.Volume{v}, true, func(ctx context.Context, v *config.Volume) error {
				return a.engine.Restore(ctx, v, snapshot)
			})
		},
	}, {
		name:  "untag",
		usage: "untag <volume> <snapshot>",
		run: func(ctx context.Context, a *app, args []string) error {
			if len(args) != 2 {
				return usagef("untag takes a volume and one of its snapshots")
			}
			volumes, err := a.poolVolumes(ctx, args[:1])
			if err != nil {
				return err
			}
			return a.each(ctx, volumes, true, func(ctx context.Context, v *config.Volume) error {
				return a.engine.Untag(ctx, v, args[1])
			})
		},
	}, {
		name:  "forget",
		usage: "forget <target>",
		run: func(ctx context.Context, a *app, args []string) error {
			if len(args) != 1 {
				return usagef("forget takes one target, given %d", len(args))
			}
			volumes, err := a.volumes(ctx, nil)
			if err != nil {
				return err
			}
			forgot := 0
			err = a.each(ctx, volumes, true, func(ctx context.Context, v *config.Volume) error {
				n, err := a.engine.Forget(ctx, v, args[0])
				forgot += n
				return err
			})
			if err == nil && forgot == 0 {
				return fmt.Errorf("no volume's pool has a record of %s", args[0])
			}
			return err
		},
	}}
}
