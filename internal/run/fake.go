package run

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Reply is the scripted result of one command.
type Reply struct {
	Out   string // standard output
	Err   error  // non-nil makes the command fail
	False bool   // makes Test report a false predicate
}

// Fake is a Runner that replays scripted replies and records what it was asked
// to do. An unscripted command is an error, so a test that exercises an
// unexpected code path fails rather than silently passing.
type Fake struct {
	HostName string
	Replies  map[string]Reply
	Calls    []string
}

// NewFake builds a fake runner for host, empty for the invoking host.
func NewFake(host string) *Fake {
	return &Fake{HostName: host, Replies: map[string]Reply{}}
}

// Script registers the reply for a command, keyed by its quoted argv.
func (f *Fake) Script(argv []string, r Reply) *Fake {
	f.Replies[Quote(argv)] = r
	return f
}

func (f *Fake) Host() string { return f.HostName }

func (f *Fake) lookup(key string) (Reply, error) {
	f.Calls = append(f.Calls, key)
	r, ok := f.Replies[key]
	if !ok {
		known := make([]string, 0, len(f.Replies))
		for k := range f.Replies {
			known = append(known, k)
		}
		sort.Strings(known)
		return Reply{}, fmt.Errorf("unscripted command on host %q: %s\nscripted:\n  %s",
			f.HostName, key, strings.Join(known, "\n  "))
	}
	return r, r.Err
}

func (f *Fake) Output(ctx context.Context, argv ...string) (string, error) {
	r, err := f.lookup(Quote(argv))
	return r.Out, err
}

func (f *Fake) Run(ctx context.Context, argv ...string) error {
	_, err := f.lookup(Quote(argv))
	return err
}

func (f *Fake) Test(ctx context.Context, argv ...string) (bool, error) {
	r, err := f.lookup(Quote(argv))
	if err != nil {
		return false, err
	}
	return !r.False, nil
}

func (f *Fake) Pipe(ctx context.Context, argv []string, dst Runner, dstArgv []string) (string, error) {
	r, err := f.lookup(PipeKey(argv, dst, dstArgv))
	return r.Out, err
}

// PipeKey names a piped pair of commands the way Fake scripts them.
func PipeKey(argv []string, dst Runner, dstArgv []string) string {
	host := dst.Host()
	if host == "" {
		host = "local"
	}
	return Quote(argv) + " | [" + host + "] " + Quote(dstArgv)
}
