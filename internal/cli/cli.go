// Package cli is gbsnap's command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/engine"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/run"
	"github.com/greendrake/gbsnap/internal/ui"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // a volume could not be dealt with
	exitUsage   = 2 // the command line or the configuration is wrong
)

// errFailed marks a run in which at least one volume failed. The failures
// themselves have already been reported by then.
var errFailed = errors.New("one or more volumes failed")

// usageError is a mistake in the command line or the configuration.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

// app is one invocation of gbsnap.
type app struct {
	printer *ui.Printer
	engine  *engine.Engine

	configPath string
	dryRun     bool
	verbose    bool
	quiet      bool
	sudoName   string
	sudo       run.SudoMode
	sudoGiven  bool

	cfg *config.Config
}

// Main runs gbsnap and returns its exit code.
func Main(args []string, out, errw io.Writer) int {
	a := &app{printer: &ui.Printer{Out: out, Err: errw}, sudoName: "auto"}

	root := flag.NewFlagSet("gbsnap", flag.ContinueOnError)
	root.SetOutput(errw)
	root.Usage = func() { usage(errw) }
	a.globals(root)
	// Asked of gbsnap itself rather than of any command, so it is not among the
	// flags every command accepts.
	showVersion := root.Bool("version", false, "print the version and exit")
	if err := root.Parse(args); err != nil {
		// Usage has been printed by now, and asking for it is not an error.
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintln(out, "gbsnap "+version)
		return exitOK
	}
	rest := root.Args()
	if len(rest) == 0 {
		usage(errw)
		return exitUsage
	}

	cmd := lookup(rest[0])
	if cmd == nil {
		a.printer.Error("unknown command %q", rest[0])
		usage(errw)
		return exitUsage
	}
	fs := flag.NewFlagSet("gbsnap "+cmd.name, flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() {
		fmt.Fprintf(errw, "usage: gbsnap %s\nRun gbsnap without arguments for the full list of flags.\n", cmd.usage)
	}
	a.globals(fs)
	if cmd.flags != nil {
		cmd.flags(fs)
	}
	args, err := parseAnywhere(fs, rest[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if err := a.settle(root, fs); err != nil {
		a.printer.Error("%v", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = cmd.run(ctx, a, args)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errFailed):
		// The failures were reported as they happened.
		return exitFailure
	}
	a.printer.Error("%v", err)
	var wrong usageError
	if errors.As(err, &wrong) {
		return exitUsage
	}
	return exitFailure
}

// parseAnywhere parses the set's flags wherever they sit among args, so that a
// flag may follow the volumes or pools it applies to, the way the usage lines
// read. It returns the non-flag arguments in order. A "--" ends flag
// recognition; everything after it is an argument.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") || i+1 == len(args) {
			continue
		}
		// A flag that is not boolean consumes the next argument as its value.
		if f := fs.Lookup(name); f != nil {
			b, ok := f.Value.(interface{ IsBoolFlag() bool })
			if !ok || !b.IsBoolFlag() {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return rest, nil
}

// globals registers the flags every command accepts. Each keeps whatever value
// an earlier flag set already gave it, so that they may appear before or after
// the command name.
func (a *app) globals(fs *flag.FlagSet) {
	fs.StringVar(&a.configPath, "config", a.configPath, "read configuration from `file`")
	fs.StringVar(&a.configPath, "c", a.configPath, "")
	fs.BoolVar(&a.dryRun, "dry-run", a.dryRun, "report what would change, and change nothing")
	fs.BoolVar(&a.dryRun, "n", a.dryRun, "")
	fs.BoolVar(&a.verbose, "verbose", a.verbose, "echo the commands gbsnap runs")
	fs.BoolVar(&a.verbose, "v", a.verbose, "")
	fs.BoolVar(&a.quiet, "quiet", a.quiet, "report errors only")
	fs.BoolVar(&a.quiet, "q", a.quiet, "")
	fs.StringVar(&a.sudoName, "sudo", a.sudoName, "acquire root `how`: auto, always or never")
}

// settle applies the parsed global flags.
func (a *app) settle(sets ...*flag.FlagSet) error {
	a.printer.Verbose, a.printer.Quiet = a.verbose, a.quiet
	for _, fs := range sets {
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "sudo" {
				a.sudoGiven = true
			}
		})
	}
	var err error
	if a.sudo, err = run.ParseSudoMode(a.sudoName); err != nil {
		return err
	}
	a.engine = engine.New(a.printer, a.dryRun)
	return nil
}

// config reads the configuration file, once.
func (a *app) config() (*config.Config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	path, err := config.Find(a.configPath)
	if err != nil {
		return nil, usageError{err}
	}
	if path == "" {
		return nil, usagef("no configuration file: looked for $GBSNAP_CONFIG, ~/.config/gbsnap.yaml and /etc/gbsnap.yaml")
	}
	if a.cfg, err = config.Load(path); err != nil {
		return nil, usageError{err}
	}
	return a.cfg, nil
}

// volumes selects the configured volumes named on the command line, or all of
// them when none is named.
func (a *app) volumes(names []string) ([]*config.Volume, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	chosen := cfg.Volumes
	if len(names) > 0 {
		chosen = nil
		for _, name := range names {
			v, err := cfg.Volume(name)
			if err != nil {
				return nil, usageError{err}
			}
			chosen = append(chosen, v)
		}
	}
	if a.sudoGiven {
		for _, v := range chosen {
			v.Sudo = a.sudo
		}
	}
	return chosen, nil
}

// pools reads command line arguments that name pools rather than volumes. The
// second result is false when the arguments name volumes.
func (a *app) pools(args []string) ([]location.Location, bool, error) {
	looksLikePool := 0
	for _, s := range args {
		if strings.ContainsAny(s, "/:") {
			looksLikePool++
		}
	}
	if looksLikePool == 0 {
		return nil, false, nil
	}
	if looksLikePool != len(args) {
		return nil, false, usagef("name either volumes or pool paths, not a mixture")
	}
	var locs []location.Location
	for _, s := range args {
		l, err := location.Parse(s)
		if err != nil {
			return nil, false, usageError{err}
		}
		locs = append(locs, l)
	}
	return locs, true, nil
}

// each works through the volumes, locking each in turn unless the work only
// reads. The lock is keyed by the volume's pool, so that any two invocations
// working on the same pool — through the configuration or ad hoc — exclude
// one another. One volume failing does not stop the others; the run as a
// whole then fails.
func (a *app) each(ctx context.Context, volumes []*config.Volume, lock bool, work func(context.Context, *config.Volume) error) error {
	failed := false
	for _, v := range volumes {
		var err error
		if lock {
			err = withLock(v.Pool.Canonical(), func() error { return work(ctx, v) })
		} else {
			err = work(ctx, v)
		}
		if err != nil {
			a.printer.Error("%s: %v", v.Name, err)
			failed = true
		}
	}
	if failed {
		return errFailed
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gbsnap replicates btrfs snapshots between pools.

usage: gbsnap [flags] <command> [flags] [arguments]

commands:
  run     [<volume>...]           snapshot, replicate and prune
  snap    [<volume>...]           snapshot each volume's subvolume, unless unchanged
  sync    [<volume>...]           replicate to every configured target
  sync    <src-pool> <dst-pool>   replicate one pool to another
  prune   [<volume>...]           retire the snapshots retention no longer covers
  prune   <pool> -min <n>         retire all but the newest <n> snapshots of a pool
  list    [<volume>|<pool>]       list snapshots
  status  [<volume>...]           show where each volume stands

A volume is named in the configuration file. A pool is a directory, written
[user@]host:path when it is reached over ssh. An argument holding a slash or a
colon is read as a pool, anything else as a volume name.

flags:
  -c, -config file   read configuration from file
                     (default $GBSNAP_CONFIG, ~/.config/gbsnap.yaml, /etc/gbsnap.yaml)
  -n, -dry-run       report what would change, and change nothing
  -v, -verbose       echo the commands gbsnap runs
  -q, -quiet         report errors only
  -sudo how          acquire root: auto (the default), always or never
  -version           print the version and exit

command flags:
  snap  -tag name    tag the new snapshot, protecting it from pruning
        -force       snapshot even if the subvolume is unchanged
  prune -min n       how many recent snapshots to keep
`)
}
