// Package pool reads and tracks the snapshots held in one directory.
package pool

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/run"
)

// ProbeName is the transient snapshot gbsnap takes to tell whether a subvolume
// has changed. The leading dot keeps it out of the pool listing.
const ProbeName = ".gbsnap-probe"

// Pool is a directory of snapshots on one host. Its listing is read once and
// then tracked in memory as gbsnap adds and removes snapshots, so a dry run can
// reason about the pool it would leave behind.
type Pool struct {
	Loc    location.Location
	runner run.Runner
	exists bool
	names  []Name
}

// New addresses a pool without reading it.
func New(loc location.Location, r run.Runner) *Pool {
	return &Pool{Loc: loc, runner: r}
}

// Runner executes commands on the pool's host.
func (p *Pool) Runner() run.Runner { return p.runner }

// Entries lists the pool directory as it stands, whatever it holds. A pool
// that does not exist yet reads as empty; Exists tells the two apart. Names
// beginning with a dot are not listed, which is what keeps the transient probe
// snapshot out of the way.
func (p *Pool) Entries(ctx context.Context) ([]string, error) {
	exists, err := p.runner.Test(ctx, "test", "-d", p.Loc.Path)
	if err != nil {
		return nil, err
	}
	p.exists = exists
	if !exists {
		return nil, nil
	}
	out, err := p.runner.Output(ctx, "ls", "-1", "--", p.Loc.Path)
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, line := range strings.Split(out, "\n") {
		if entry := strings.TrimSpace(line); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// Load reads the pool's listing. A pool that does not exist yet reads as empty.
func (p *Pool) Load(ctx context.Context) error {
	entries, err := p.Entries(ctx)
	if err != nil {
		return err
	}
	p.names = nil
	var strays []string
	for _, entry := range entries {
		n, err := ParseName(entry)
		if err != nil {
			strays = append(strays, entry)
			continue
		}
		p.names = append(p.names, n)
	}
	if len(strays) > 0 {
		return fmt.Errorf("pool %s holds entries that are not snapshots: %s", p.Loc, strings.Join(strays, ", "))
	}
	p.sort()
	return nil
}

func (p *Pool) sort() {
	slices.SortFunc(p.names, func(a, b Name) int {
		switch {
		case a.Before(b):
			return -1
		case b.Before(a):
			return 1
		}
		return 0
	})
}

// Exists reports whether the pool directory is there.
func (p *Pool) Exists() bool { return p.exists }

// Names lists the pool's snapshots, oldest first.
func (p *Pool) Names() []Name { return p.names }

// Latest is the newest snapshot in the pool.
func (p *Pool) Latest() (Name, bool) {
	if len(p.names) == 0 {
		return Name{}, false
	}
	return p.names[len(p.names)-1], true
}

// Path addresses a snapshot on the pool's host.
func (p *Pool) Path(n Name) string { return path.Join(p.Loc.Path, n.String()) }

// ProbePath addresses the transient change-detection snapshot.
func (p *Pool) ProbePath() string { return path.Join(p.Loc.Path, ProbeName) }

// Create makes the pool directory.
func (p *Pool) Create(ctx context.Context) error {
	if err := p.runner.Run(ctx, "mkdir", "-p", "--", p.Loc.Path); err != nil {
		return err
	}
	p.exists = true
	return nil
}

// Add records a snapshot gbsnap has just created or received.
func (p *Pool) Add(n Name) {
	p.names = append(p.names, n)
	p.exists = true
	p.sort()
}

// Remove forgets a snapshot gbsnap has just deleted.
func (p *Pool) Remove(n Name) {
	p.names = slices.DeleteFunc(p.names, func(e Name) bool { return e.String() == n.String() })
}

// Common finds the newest snapshot held by both pools, which is the parent an
// incremental send can build on.
func Common(src, dst []Name) (Name, bool) {
	held := map[string]bool{}
	for _, n := range dst {
		held[n.String()] = true
	}
	for i := len(src) - 1; i >= 0; i-- {
		if held[src[i].String()] {
			return src[i], true
		}
	}
	return Name{}, false
}

// After lists the snapshots newer than n, or all of them when there is no n.
func After(names []Name, n Name, has bool) []Name {
	if !has {
		return slices.Clone(names)
	}
	var out []Name
	for _, e := range names {
		if n.Before(e) {
			out = append(out, e)
		}
	}
	return out
}
