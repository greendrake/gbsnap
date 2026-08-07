package engine

// Conversion from the naming this tool's predecessor wrote. This file goes
// when no pool is left carrying it.

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/greendrake/gbsnap/internal/btrfs"
	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
)

// Convert renames the snapshots of a volume's pool, and of every one of its
// targets, out of the predecessor's naming and into this tool's.
//
// Both sides are done in one go deliberately: a snapshot is paired with its
// copy by name alone, so converting a pool without its targets would leave the
// two unable to recognise what they already share, and the next sync would
// begin again from nothing.
func (e *Engine) Convert(ctx context.Context, v *config.Volume) error {
	for _, loc := range append([]location.Location{v.Pool}, v.Targets...) {
		if err := e.convertPool(ctx, v, loc); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) convertPool(ctx context.Context, v *config.Volume, loc location.Location) error {
	p := pool.New(loc, e.runner(loc, v.Sudo))
	entries, err := p.Entries(ctx)
	if err != nil {
		return err
	}
	if !p.Exists() {
		if v.AdHoc {
			return fmt.Errorf("pool %s does not exist", loc)
		}
		e.printer.Detail("%s: pool %s does not exist, so there is nothing to convert", v.Name, loc)
		return nil
	}

	// The two namings cannot be mistaken for one another — one carries dashes
	// in its date, the other a T and a Z — so what an entry parses as is what
	// it is. Anything that parses as neither stops the conversion, rather than
	// being renamed on a guess.
	taken := map[string]bool{}
	rename := map[string]pool.Name{}
	var unknown []string
	for _, entry := range entries {
		if _, err := pool.ParseName(entry); err == nil {
			taken[entry] = true
			continue
		}
		n, err := pool.ParseLegacyName(entry)
		if err != nil {
			unknown = append(unknown, entry)
			continue
		}
		rename[entry] = n
	}
	if len(unknown) > 0 {
		return fmt.Errorf("pool %s holds entries this tool wrote neither now nor before: %s",
			loc, strings.Join(unknown, ", "))
	}
	if len(rename) == 0 {
		e.printer.Detail("%s: pool %s is already in this tool's naming", v.Name, loc)
		return nil
	}
	// Sorting by the old name is sorting by date and sequence, so the pool is
	// worked through oldest first and reads that way in a dry run.
	order := slices.Sorted(maps.Keys(rename))

	// No two entries may converge on one name, and none may claim a name the
	// pool already holds. Checked in full before anything moves, so that a
	// pool gbsnap cannot convert cleanly is left exactly as it was. The two
	// ways of colliding are reported apart, because they are put right
	// differently: one entry is in the way, or two are the same snapshot named
	// twice.
	claimed := map[string]string{}
	for _, from := range order {
		to := rename[from].String()
		if taken[to] {
			return fmt.Errorf("pool %s: %s would become %s, which the pool already holds", loc, from, to)
		}
		if other, ok := claimed[to]; ok {
			return fmt.Errorf("pool %s: %s and %s would both become %s", loc, other, from, to)
		}
		claimed[to] = from
	}

	// Adopting something that is not a read-only snapshot would buy a failure
	// later, at the send that cannot stream it, in exchange for a rename now.
	for _, from := range order {
		sub, err := btrfs.Show(ctx, p.Runner(), path.Join(loc.Path, from))
		if err != nil {
			return err
		}
		if !sub.ReadOnly {
			return fmt.Errorf("%s is not a read-only snapshot, so this tool could never send it", loc.Child(from))
		}
	}

	for _, from := range order {
		to := rename[from].String()
		if err := e.do(fmt.Sprintf("rename %s to %s", loc.Child(from), to), func() error {
			// -T so that a name already taken is never quietly turned into a
			// move of the snapshot into that directory.
			return p.Runner().Run(ctx, "mv", "-T", "--",
				path.Join(loc.Path, from), path.Join(loc.Path, to))
		}); err != nil {
			return err
		}
	}
	return nil
}
