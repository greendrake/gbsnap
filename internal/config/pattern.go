package config

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/run"
)

// pattern is an entry standing for every subvolume directly inside one
// directory, each a volume of its own, named after the subvolume. Its pool and
// any targets of its own are written with a * where that name goes.
type pattern struct {
	key      string
	dir      location.Location // where the subvolumes are looked for
	pool     string
	targets  []string // nil: a pool named after each volume in every place
	places   []location.Location
	exclude  []string
	own      settings // the entry as written
	defaults settings
	sudo     run.SudoMode
	at       int // how many plain volumes precede it, for its matches to follow
}

func newPattern(key string, s settings, defaults settings, places []location.Location) (*pattern, error) {
	if s.Pool == "" {
		return nil, errors.New("no pool")
	}
	dir := strings.TrimSuffix(s.Subvolume, "/*")
	if strings.Contains(dir, "*") {
		return nil, fmt.Errorf("subvolume %s: only the last element may be a *", s.Subvolume)
	}
	if strings.Count(s.Pool, "*") != 1 {
		return nil, fmt.Errorf("pool %s: want one * where each volume's name goes", s.Pool)
	}
	for _, t := range s.Targets {
		if strings.Count(t, "*") != 1 {
			return nil, fmt.Errorf("target %s: want one * where each volume's name goes, or every volume would share one pool", t)
		}
	}
	for _, name := range s.Exclude {
		if err := nameProblem(name); err != nil {
			return nil, fmt.Errorf("exclude: %w", err)
		}
	}
	dirLoc, err := location.Parse(dir)
	if err != nil {
		return nil, err
	}
	p := &pattern{key: key, dir: dirLoc, pool: s.Pool, targets: s.Targets, places: places,
		exclude: s.Exclude, own: s, defaults: defaults}
	// Whatever would be wrong with every match is wrong with the pattern, and
	// is reported now rather than only once something matches.
	sample, err := p.volume("x")
	if err != nil {
		return nil, err
	}
	p.sudo = sample.Sudo
	return p, nil
}

// volume builds the volume one matching subvolume stands for.
func (p *pattern) volume(name string) (*Volume, error) {
	s := settings{
		Subvolume: p.dir.Child(name).String(),
		Pool:      strings.Replace(p.pool, "*", name, 1),
		Retention: p.own.Retention,
		Sudo:      p.own.Sudo,
	}
	if p.targets != nil {
		s.Targets = []string{}
		for _, t := range p.targets {
			s.Targets = append(s.Targets, strings.Replace(t, "*", name, 1))
		}
	}
	return build(name, s, p.defaults, p.places)
}

// Lister finds the subvolumes directly inside a directory whose names do not
// start with a dot.
type Lister func(ctx context.Context, dir location.Location, sudo run.SudoMode) ([]string, error)

// Expand finds the volumes the configuration's patterns stand for, and puts
// them among the others where each pattern was written. Two volumes may not
// share a name, whether written out or found.
func (c *Config) Expand(ctx context.Context, list Lister) error {
	if c.expanded || len(c.patterns) == 0 {
		c.expanded = true
		return nil
	}
	var volumes []*Volume
	next := 0
	for _, p := range c.patterns {
		volumes = append(volumes, c.Volumes[next:p.at]...)
		next = p.at
		names, err := list(ctx, p.dir, p.sudo)
		if err != nil {
			return fmt.Errorf("volume %q: finding the subvolumes in %s: %w", p.key, p.dir, err)
		}
		slices.Sort(names)
		for _, name := range names {
			if slices.Contains(p.exclude, name) || strings.HasPrefix(name, ".") {
				continue
			}
			if err := nameProblem(name); err != nil {
				return fmt.Errorf("volume %q: subvolume %s: %w", p.key, p.dir.Child(name), err)
			}
			v, err := p.volume(name)
			if err != nil {
				return fmt.Errorf("volume %q: %s: %w", p.key, name, err)
			}
			volumes = append(volumes, v)
		}
	}
	volumes = append(volumes, c.Volumes[next:]...)

	seen := map[string]bool{}
	for _, v := range volumes {
		if seen[v.Name] {
			return fmt.Errorf("two volumes are named %q; a pattern found one of them, so exclude it there", v.Name)
		}
		seen[v.Name] = true
	}
	c.Volumes, c.expanded = volumes, true
	return nil
}

// Absent builds the volume a pattern would stand for, were its subvolume
// there: what restoring one that is gone, or was never on this machine, starts
// from. The first pattern that would take the name, and does not exclude it,
// has it.
func (c *Config) Absent(name string) (*Volume, error) {
	if err := nameProblem(name); err != nil {
		return nil, err
	}
	for _, p := range c.patterns {
		if !slices.Contains(p.exclude, name) && !strings.HasPrefix(name, ".") {
			return p.volume(name)
		}
	}
	return nil, fmt.Errorf("no volume %q in %s", name, c.Path)
}
