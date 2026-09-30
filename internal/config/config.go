// Package config reads the volumes gbsnap looks after.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// Config is a whole configuration, read from one file or several.
type Config struct {
	// Path names the files it was read from.
	Path    string
	Volumes []*Volume
	// patterns are the entries standing for volumes to be found, which Expand
	// looks for.
	patterns []*pattern
	expanded bool
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

// Find returns the files a configuration is read from, in the order they are
// read: a configuration file, then the drop-ins in the .d directory beside it
// (gbsnap.d beside gbsnap.yaml), in name order. An explicit path is used as
// given; otherwise the usual places are tried in turn, and the first where
// either the file or its .d directory is there wins. The result is empty when
// there is no configuration anywhere.
func Find(explicit string) ([]string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return nil, err
		}
		return withDropIns(explicit)
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
		files, err := withDropIns(c)
		if err != nil || len(files) > 0 {
			return files, err
		}
	}
	return nil, nil
}

// DropIns is the directory of drop-ins beside a configuration file.
func DropIns(file string) string {
	return strings.TrimSuffix(file, filepath.Ext(file)) + ".d"
}

// withDropIns lists a configuration file, if it is there, and the drop-ins
// beside it.
func withDropIns(file string) ([]string, error) {
	var files []string
	if _, err := os.Stat(file); err == nil {
		files = append(files, file)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	dir := DropIns(file)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && (ext == ".yaml" || ext == ".yml") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

// Load reads and validates a configuration from its files, in order.
func Load(paths ...string) (*Config, error) {
	var pieces []piece
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		p, err := readPiece(path, data)
		if err != nil {
			return nil, err
		}
		pieces = append(pieces, p)
	}
	cfg, err := assemble(pieces)
	if err != nil {
		return nil, err
	}
	cfg.Path = strings.Join(paths, ", ")
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

// file mirrors the shape of one configuration file.
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
// volume's paths, save targets: there they name places, each volume's target
// being a pool named after it in each.
type settings struct {
	Subvolume string           `yaml:"subvolume"`
	Pool      string           `yaml:"pool"`
	Targets   []string         `yaml:"targets"`
	Exclude   []string         `yaml:"exclude"`
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

// piece is one file of a configuration, as written.
type piece struct {
	path     string
	defaults settings
	names    []string // the volumes, in the order written
	volumes  map[string]settings
}

// fail puts the file's name in front of an error, when there is a file.
func (p piece) fail(err error) error {
	if p.path == "" {
		return err
	}
	return fmt.Errorf("%s: %w", p.path, err)
}

// parse reads a configuration held in one file.
func parse(data []byte) (*Config, error) {
	p, err := readPiece("", data)
	if err != nil {
		return nil, err
	}
	return assemble([]piece{p})
}

func readPiece(path string, data []byte) (piece, error) {
	p := piece{path: path}
	// Strict decoding refuses keys the configuration has no meaning for, so a
	// misspelt setting is reported instead of quietly doing nothing.
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return p, p.fail(err)
	}
	if f.Defaults.Subvolume != "" || f.Defaults.Pool != "" || f.Defaults.Exclude != nil {
		return p, p.fail(errors.New("defaults: subvolume, pool and exclude belong to a volume, not to the defaults"))
	}
	p.defaults, p.volumes = f.Defaults, f.Volumes

	var ord order
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&ord); err != nil && !errors.Is(err, io.EOF) {
		return p, p.fail(err)
	}
	if ord.Volumes.Kind == 0 {
		return p, nil // a file of defaults alone
	}
	if ord.Volumes.Kind != yaml.MappingNode {
		return p, p.fail(errors.New("volumes: want a mapping of name to volume"))
	}
	// A mapping node alternates keys and values in the order they were written.
	for i := 0; i+1 < len(ord.Volumes.Content); i += 2 {
		name := ord.Volumes.Content[i].Value
		if err := nameProblem(name); err != nil {
			return p, p.fail(fmt.Errorf("volumes: %w", err))
		}
		p.names = append(p.names, name)
	}
	return p, nil
}

// nameProblem says what keeps a name from naming a volume, if anything.
func nameProblem(name string) error {
	if name == "" {
		return errors.New("a volume has an empty name")
	}
	// The command line reads an argument holding a slash or a colon as a pool
	// path, and one starting with a dash as a flag, so such a name could never
	// be addressed.
	if strings.ContainsAny(name, "/:") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("%q cannot be named on the command line; a name must not start with a dash or contain a slash or a colon", name)
	}
	return nil
}

// assemble puts the pieces of a configuration together. Each may set
// defaults, a later piece's winning over an earlier's, and each may add
// volumes, which the defaults apply to wherever they were written. No one
// piece has to hold volumes, but the whole has to.
func assemble(pieces []piece) (*Config, error) {
	var defaults settings
	for _, p := range pieces {
		mergeDefaults(&defaults, p.defaults)
	}
	places, err := parseAll(defaults.Targets)
	if err != nil {
		return nil, fmt.Errorf("defaults: targets: %w", err)
	}

	cfg := &Config{}
	from := map[string]string{}
	for _, p := range pieces {
		for _, name := range p.names {
			if earlier, ok := from[name]; ok {
				return nil, fmt.Errorf("volume %q is in both %s and %s", name, where(earlier), where(p.path))
			}
			from[name] = p.path
			s := p.volumes[name]
			if strings.HasSuffix(s.Subvolume, "/*") {
				pat, err := newPattern(name, s, defaults, places)
				if err != nil {
					return nil, p.fail(fmt.Errorf("volume %q: %w", name, err))
				}
				pat.at = len(cfg.Volumes)
				cfg.patterns = append(cfg.patterns, pat)
				continue
			}
			v, err := build(name, s, defaults, places)
			if err != nil {
				return nil, p.fail(fmt.Errorf("volume %q: %w", name, err))
			}
			cfg.Volumes = append(cfg.Volumes, v)
		}
	}
	if len(cfg.Volumes) == 0 && len(cfg.patterns) == 0 {
		return nil, errors.New("no volumes")
	}
	return cfg, nil
}

func where(path string) string {
	if path == "" {
		return "the configuration"
	}
	return path
}

// mergeDefaults lays a later piece's defaults over those gathered so far.
func mergeDefaults(into *settings, later settings) {
	if later.Targets != nil {
		into.Targets = later.Targets
	}
	if later.Sudo != "" {
		into.Sudo = later.Sudo
	}
	if later.Retention != nil {
		if into.Retention == nil {
			into.Retention = &retentionConfig{}
		}
		if p := later.Retention.pool(); p != nil && p.Min != nil {
			into.Retention.Pool = p
		}
		if t := later.Retention.target(); t != nil && t.Min != nil {
			into.Retention.Target = t
		}
	}
}

func parseAll(texts []string) ([]location.Location, error) {
	var locs []location.Location
	for _, t := range texts {
		l, err := location.Parse(t)
		if err != nil {
			return nil, err
		}
		locs = append(locs, l)
	}
	return locs, nil
}

func build(name string, s, defaults settings, places []location.Location) (*Volume, error) {
	if s.Pool == "" {
		return nil, errors.New("no pool")
	}
	if s.Exclude != nil {
		return nil, errors.New("exclude belongs to a pattern, whose subvolume ends in /*")
	}
	for _, text := range append([]string{s.Subvolume, s.Pool}, s.Targets...) {
		if strings.Contains(text, "*") {
			return nil, fmt.Errorf("%s: a * belongs to a pattern, whose subvolume ends in /*", text)
		}
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

	// A volume's own targets win, even an empty list of them; otherwise it
	// gets a pool named after it in each place the defaults name.
	var targets []location.Location
	if s.Targets != nil {
		if targets, err = parseAll(s.Targets); err != nil {
			return nil, err
		}
	} else {
		for _, place := range places {
			targets = append(targets, place.Child(name))
		}
	}
	for _, target := range targets {
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
