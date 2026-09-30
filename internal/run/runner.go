// Package run executes commands on the invoking host or behind ssh.
//
// Commands are always described as an argv slice; no command line is ever
// assembled by string concatenation. For a remote host the argv is quoted into
// the single argument ssh passes to the remote shell.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/greendrake/gbsnap/internal/ui"
)

// Runner executes commands on one host.
type Runner interface {
	// Output runs argv and returns its standard output.
	Output(ctx context.Context, argv ...string) (string, error)
	// Run runs argv and discards its standard output.
	Run(ctx context.Context, argv ...string) error
	// Test runs a predicate, mapping exit status 0 to true and 1 to false.
	// Any other exit status is an error.
	Test(ctx context.Context, argv ...string) (bool, error)
	// Input runs argv with stdin as its standard input, discarding its output.
	Input(ctx context.Context, stdin string, argv ...string) error
	// Pipe runs argv here and streams its standard output into dstArgv on dst,
	// returning the standard output of dstArgv.
	Pipe(ctx context.Context, argv []string, dst Runner, dstArgv []string) (string, error)
	// Host names the host, empty for the invoking one.
	Host() string
}

// SudoMode selects how commands acquire root.
type SudoMode int

const (
	// SudoAuto uses sudo only where the login is not already root.
	SudoAuto SudoMode = iota
	SudoAlways
	SudoNever
)

var sudoModeNames = map[string]SudoMode{
	"auto":   SudoAuto,
	"always": SudoAlways,
	"never":  SudoNever,
}

// ParseSudoMode reads the auto, always or never keyword.
func ParseSudoMode(s string) (SudoMode, error) {
	m, ok := sudoModeNames[s]
	if !ok {
		return 0, fmt.Errorf("sudo: want auto, always or never, got %q", s)
	}
	return m, nil
}

func (m SudoMode) String() string {
	for name, v := range sudoModeNames {
		if v == m {
			return name
		}
	}
	return "unknown"
}

// Error carries the command that failed together with what it wrote to stderr.
type Error struct {
	Host   string
	Argv   []string
	Stderr string
	Err    error
	// Unreachable is set when ssh itself failed, so that the command never
	// ran: ssh exits 255 when it cannot connect or log in.
	Unreachable bool
}

func (e *Error) Error() string {
	where := "locally"
	if e.Host != "" {
		where = "on " + e.Host
	}
	msg := fmt.Sprintf("running %s %s: %v", Quote(e.Argv), where, e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Unreachable reports whether err is ssh failing to reach its host, as opposed
// to a command that ran there and failed.
func Unreachable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Unreachable
}

// sshFailed is the exit status ssh reserves for its own failures.
const sshFailed = 255

// failure builds the Error for a command that failed on this runner's host.
func (e *Exec) failure(argv []string, stderr string, err error) *Error {
	var exitErr *exec.ExitError
	unreachable := e.host != "" && errors.As(err, &exitErr) && exitErr.ExitCode() == sshFailed
	return &Error{Host: e.host, Argv: argv, Stderr: stderr, Err: err, Unreachable: unreachable}
}

// Exec runs commands on one host, locally or over ssh, optionally under sudo.
type Exec struct {
	host     string
	sudo     SudoMode
	printer  *ui.Printer
	resolved *bool // memoised SudoAuto decision
	checked  bool  // sudo confirmed working

	stopOnce  sync.Once
	stop      chan struct{} // closed by Close, ending the keep-alive
	keepAlive bool          // the keep-alive has been started
}

// NewExec builds a runner for host, empty for the invoking host.
func NewExec(host string, sudo SudoMode, printer *ui.Printer) *Exec {
	return &Exec{host: host, sudo: sudo, printer: printer, stop: make(chan struct{})}
}

// KeepAliveEvery is how often a local sudo's cached credentials are refreshed
// while gbsnap works. sudo forgets them after five minutes by default.
var KeepAliveEvery = time.Minute

// startKeepAlive refreshes the local sudo's cached credentials now and then
// until the runner is closed. One command can outlast sudo's timeout — a first
// full send can take hours — and the command after it would then ask for a
// password nobody is there to type. A remote sudo gets no such help: with no
// terminal to ask on, it has to need no password at all.
func (e *Exec) startKeepAlive() {
	if e.host != "" || e.keepAlive {
		return
	}
	e.keepAlive = true
	go func() {
		tick := time.NewTicker(KeepAliveEvery)
		defer tick.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-tick.C:
				// -n: refresh what is cached, never ask. A refresh that fails
				// leaves the next command to report sudo's complaint.
				_ = exec.Command("sudo", "-n", "-v").Run()
			}
		}
	}()
}

// Close ends the runner's keep-alive, if it has one.
func (e *Exec) Close() error {
	e.stopOnce.Do(func() { close(e.stop) })
	return nil
}

func (e *Exec) Host() string { return e.host }

// useSudo resolves SudoAuto by asking the host who it is, once, and confirms
// once that sudo actually works before it is relied on: sudo's own failures
// exit with the status 1 that Test reads as a predicate answering false, so
// they have to be ruled out up front.
func (e *Exec) useSudo(ctx context.Context) (bool, error) {
	switch e.sudo {
	case SudoNever:
		return false, nil
	case SudoAuto:
		if e.resolved == nil {
			need := false
			if e.host == "" {
				need = os.Geteuid() != 0
			} else {
				probe := []string{"id", "-u"}
				out, err := e.capture(e.prepare(ctx, e.wrap(false, probe)), probe)
				if err != nil {
					return false, err
				}
				need = strings.TrimSpace(out) != "0"
			}
			e.resolved = &need
		}
		if !*e.resolved {
			return false, nil
		}
	}
	if !e.checked {
		argv := []string{"sudo", "true"}
		if _, err := e.capture(e.prepare(ctx, e.wrap(false, argv)), argv); err != nil {
			if Unreachable(err) {
				return false, err
			}
			// %v, not %w: the probe's exit status must not reach Test, which
			// reads an exit status of 1 as a predicate answering false.
			return false, fmt.Errorf("sudo is needed but does not work: %v", err)
		}
		e.checked = true
		e.startKeepAlive()
	}
	return true, nil
}

// wrap turns a command for this host into an argv for the invoking host.
func (e *Exec) wrap(sudo bool, argv []string) []string {
	if sudo {
		argv = append([]string{"sudo"}, argv...)
	}
	if e.host != "" {
		argv = []string{"ssh", e.host, Quote(argv)}
	}
	return argv
}

// prepare builds the invoking-host command and echoes it under --verbose.
func (e *Exec) prepare(ctx context.Context, full []string) *exec.Cmd {
	e.printer.Detail("+ %s", Quote(full))
	return exec.CommandContext(ctx, full[0], full[1:]...)
}

// command prepares argv for execution on this host.
func (e *Exec) command(ctx context.Context, argv []string) (*exec.Cmd, error) {
	sudo, err := e.useSudo(ctx)
	if err != nil {
		return nil, err
	}
	return e.prepare(ctx, e.wrap(sudo, argv)), nil
}

// capture runs cmd to completion, keeping stdout and stderr apart. Standard
// input is left closed so that sudo and ssh fail instead of waiting for a
// password that nothing is there to type.
func (e *Exec) capture(cmd *exec.Cmd, argv []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), e.failure(argv, stderr.String(), err)
	}
	return stdout.String(), nil
}

func (e *Exec) Output(ctx context.Context, argv ...string) (string, error) {
	cmd, err := e.command(ctx, argv)
	if err != nil {
		return "", err
	}
	return e.capture(cmd, argv)
}

func (e *Exec) Run(ctx context.Context, argv ...string) error {
	_, err := e.Output(ctx, argv...)
	return err
}

func (e *Exec) Input(ctx context.Context, stdin string, argv ...string) error {
	cmd, err := e.command(ctx, argv)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(stdin)
	_, err = e.capture(cmd, argv)
	return err
}

func (e *Exec) Test(ctx context.Context, argv ...string) (bool, error) {
	_, err := e.Output(ctx, argv...)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func (e *Exec) Pipe(ctx context.Context, argv []string, dst Runner, dstArgv []string) (string, error) {
	de, ok := dst.(*Exec)
	if !ok {
		return "", fmt.Errorf("cannot pipe into a %T", dst)
	}
	src, err := e.command(ctx, argv)
	if err != nil {
		return "", err
	}
	sink, err := de.command(ctx, dstArgv)
	if err != nil {
		return "", err
	}

	// A real pipe between the two processes: the stream never passes through
	// this process, and each side keeps its own stderr.
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	var srcErr, dstErr, dstOut bytes.Buffer
	src.Stdout, src.Stderr = pw, &srcErr
	sink.Stdin, sink.Stdout, sink.Stderr = pr, &dstOut, &dstErr

	if err := src.Start(); err != nil {
		pr.Close()
		pw.Close()
		return "", &Error{Host: e.host, Argv: argv, Err: err}
	}
	if err := sink.Start(); err != nil {
		pr.Close()
		pw.Close()
		src.Process.Kill()
		src.Wait()
		return "", &Error{Host: de.host, Argv: dstArgv, Err: err}
	}
	// Both ends belong to the children now; the sink only reaches end-of-stream
	// once this process lets go of the write end.
	pw.Close()
	pr.Close()

	srcDone := make(chan error, 1)
	go func() { srcDone <- src.Wait() }()
	sinkWait := sink.Wait()
	srcWait := <-srcDone

	// The sender is reported first: when it fails the receiver only ever sees a
	// truncated stream, and its complaint would bury the real cause.
	if srcWait != nil {
		return dstOut.String(), e.failure(argv, srcErr.String(), srcWait)
	}
	if sinkWait != nil {
		return dstOut.String(), de.failure(dstArgv, dstErr.String(), sinkWait)
	}
	return dstOut.String(), nil
}

// Quote renders argv as a single shell word sequence, safe to hand to a remote
// shell through ssh.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArg(a)
	}
	return strings.Join(parts, " ")
}

// unquoted lists the characters a POSIX shell leaves alone.
const unquoted = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-"

func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune(unquoted, r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
