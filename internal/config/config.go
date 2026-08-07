// Package config reads the volumes gbsnap looks after.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/retention"
	"github.com/greendrake/gbsnap/internal/run"
)

// DefaultMin is how many recent snapshots a pool keeps when the file is silent.
const DefaultMin = 3

// Config is a whole configuration file.
type Config struct {
	Path    string
	Volumes []*Volume
}

// Volume wires one subvolume to the pool that holds its snapshots and the
// targets that copy them.
type Volume struct {
	Name string
	// Subvolume is the live data. It is absent for a pool that something else
	// fills, which gbsnap only replicates and prunes.
	Subvolume *location.Location
	Pool      location.Location
	Targets   []location.Location
	Retention Retention
	Sudo      run.SudoMode
	// AdHoc marks a volume assembled from pool paths on the command line
	// rather than the configuration file. Its pool has to exist already: a
	// mistyped path must not read as an empty pool and succeed.
	AdHoc bool
}

// Retention is how much history each side of a volume keeps.
type Retention struct {
	Pool   retention.Policy
	Target retention.Policy
}

// Find returns the configuration file to read. An explicit path is used as
// given; otherwise the usual places are tried in turn. The result is empty
// when there is no configuration file anywhere.
func Find(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	var candidates []string
	if fromEnv := os.Getenv("GBSNAP_CONFIG"); fromEnv != "" {
		candidates = append(candidates, fromEnv)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "gbsnap.yaml"))
	}
	candidates = append(candidates, "/etc/gbsnap.yaml")
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", nil
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	return cfg, nil
}

// AdHoc builds a volume standing for pools named on the command line rather
// than in a file. It has no subvolume, so it is only good for replicating and
// pruning what other runs have already snapshotted.
func AdHoc(poolLoc location.Location, targets []location.Location, min int, sudo run.SudoMode) *Volume {
	return &Volume{
		Name:      poolLoc.String(),
		Pool:      poolLoc,
		Targets:   targets,
		Retention: Retention{Pool: retention.Policy{Min: min}, Target: retention.Policy{Min: min}},
		Sudo:      sudo,
		AdHoc:     true,
	}
}

// Volume finds a volume by name.
func (c *Config) Volume(name string) (*Volume, error) {
	for _, v := range c.Volumes {
		if v.Name == name {
			return v, nil
		}
	}
	return nil, fmt.Errorf("no volume %q in %s", name, c.Path)
}

// file mirrors the shape of the configuration file.
type file struct {
	Defaults settings            `yaml:"defaults"`
	Volumes  map[string]settings `yaml:"volumes"`
}

// order recovers the sequence the volumes were written in, which a map loses.
type order struct {
	Volumes yaml.Node `yaml:"volumes"`
}

// settings is the set of keys a volume may carry. The defaults block shares
// the shape but may only carry the keys that are not about a particular
// volume's paths.
type settings struct {
	Subvolume string           `yaml:"subvolume"`
	Pool      string           `yaml:"pool"`
	Targets   []string         `yaml:"targets"`
	Retention *retentionConfig `yaml:"retention"`
	Sudo      string           `yaml:"sudo"`
}

type retentionConfig struct {
	Pool   *policyConfig `yaml:"pool"`
	Target *policyConfig `yaml:"target"`
}

// The accessors below read through a block that was never written.
func (r *retentionConfig) pool() *policyConfig {
	if r == nil {
		return nil
	}
	return r.Pool
}

func (r *retentionConfig) target() *policyConfig {
	if r == nil {
		return nil
	}
	return r.Target
}

type policyConfig struct {
	Min *int `yaml:"min"`
}

func parse(data []byte) (*Config, error) {
	// Strict decoding refuses keys the configuration has no meaning for, so a
	// misspelt setting is reported instead of quietly doing nothing.
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if f.Defaults.Subvolume != "" || f.Defaults.Pool != "" || f.Defaults.Targets != nil {
		return nil, errors.New("defaults: subvolume, pool and targets belong to a volume, not to the defaults")
	}
	if len(f.Volumes) == 0 {
		return nil, errors.New("no volumes")
	}

	var ord order
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&ord); err != nil {
		return nil, err
	}
	if ord.Volumes.Kind != yaml.MappingNode {
		return nil, errors.New("volumes: want a mapping of name to volume")
	}

	cfg := &Config{}
	// A mapping node alternates keys and values in the order they were written.
	for i := 0; i+1 < len(ord.Volumes.Content); i += 2 {
		name := ord.Volumes.Content[i].Value
		if name == "" {
			return nil, errors.New("volumes: a volume has an empty name")
		}
		// The command line reads an argument holding a slash or a colon as a
		// pool path, and one starting with a dash as a flag, so such a name
		// could never be addressed.
		if strings.ContainsAny(name, "/:") || strings.HasPrefix(name, "-") {
			return nil, fmt.Errorf("volumes: %q cannot be named on the command line; a name must not start with a dash or contain a slash or a colon", name)
		}
		v, err := build(name, f.Volumes[name], f.Defaults)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", name, err)
		}
		cfg.Volumes = append(cfg.Volumes, v)
	}
	return cfg, nil
}

func build(name string, s, defaults settings) (*Volume, error) {
	if s.Pool == "" {
		return nil, errors.New("no pool")
	}
	poolLoc, err := location.Parse(s.Pool)
	if err != nil {
		return nil, err
	}
	v := &Volume{Name: name, Pool: poolLoc}

	if s.Subvolume != "" {
		sub, err := location.Parse(s.Subvolume)
		if err != nil {
			return nil, err
		}
		// A snapshot can only be taken within one filesystem, so a subvolume
		// and its pool must at least share a host. The prefix is compared as
		// written, since that is also what addresses the host to ssh.
		if sub.Host != poolLoc.Host {
			return nil, fmt.Errorf("subvolume %s and pool %s are on different hosts, so no snapshot could be taken: "+
				"write both as plain paths, or give both the same host prefix, spelled alike "+
				"(user@host and host count as different hosts)", sub, poolLoc)
		}
		v.Subvolume = &sub
	}

	for _, t := range s.Targets {
		target, err := location.Parse(t)
		if err != nil {
			return nil, err
		}
		if target.Equal(poolLoc) {
			return nil, fmt.Errorf("target %s is the pool itself", target)
		}
		for _, already := range v.Targets {
			if already.Equal(target) {
				return nil, fmt.Errorf("target %s is listed twice", target)
			}
		}
		v.Targets = append(v.Targets, target)
	}

	sudoText := "auto"
	if defaults.Sudo != "" {
		sudoText = defaults.Sudo
	}
	if s.Sudo != "" {
		sudoText = s.Sudo
	}
	if v.Sudo, err = run.ParseSudoMode(sudoText); err != nil {
		return nil, err
	}

	if v.Retention.Pool, err = policy(s.Retention.pool(), defaults.Retention.pool()); err != nil {
		return nil, fmt.Errorf("retention pool: %w", err)
	}
	if v.Retention.Target, err = policy(s.Retention.target(), defaults.Retention.target()); err != nil {
		return nil, fmt.Errorf("retention target: %w", err)
	}
	return v, nil
}

// policy settles one side's retention, the volume's own setting winning over
// the defaults and both over the built-in one.
func policy(own, defaults *policyConfig) (retention.Policy, error) {
	p := retention.Policy{Min: DefaultMin}
	if defaults != nil && defaults.Min != nil {
		p.Min = *defaults.Min
	}
	if own != nil && own.Min != nil {
		p.Min = *own.Min
	}
	if p.Min < 1 {
		return p, fmt.Errorf("min is %d, which would leave the pool empty; want at least 1", p.Min)
	}
	return p, nil
}
