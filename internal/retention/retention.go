// Package retention decides which snapshots a pool may let go of.
package retention

import "github.com/greendrake/gbsnap/internal/pool"

// Policy is how much history a pool keeps.
type Policy struct {
	// Min is how many recent prunable snapshots survive.
	Min int
}

// Select lists the snapshots to delete, oldest first. Names must be ordered
// oldest first, as a loaded pool provides them.
//
// Two kinds of snapshot are beyond the policy's reach: tagged ones, which are
// protected for as long as they carry a tag, and pinned ones, which are the
// snapshots a target still needs as the parent of its next incremental send.
// Neither counts towards Min, so protecting or pinning a snapshot adds to the
// history rather than pushing a recent snapshot out of it.
func Select(names []pool.Name, p Policy, pinned map[string]bool) []pool.Name {
	var prunable []pool.Name
	for _, n := range names {
		if n.Protected() || pinned[n.String()] {
			continue
		}
		prunable = append(prunable, n)
	}
	if len(prunable) <= p.Min {
		return nil
	}
	return prunable[:len(prunable)-p.Min]
}
