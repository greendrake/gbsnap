package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/ui"
)

func quiet() *ui.Printer { return &ui.Printer{Out: io.Discard, Err: io.Discard} }

func TestQuote(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"btrfs", "subvolume", "show", "/pool/snap"}, "btrfs subvolume show /pool/snap"},
		{[]string{"ls", "-1", "--", "/pool/my snaps"}, `ls -1 -- '/pool/my snaps'`},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"echo", ""}, "echo ''"},
		{[]string{"echo", "a;rm -rf /"}, `echo 'a;rm -rf /'`},
		{[]string{"echo", "$HOME"}, `echo '$HOME'`},
		{[]string{"echo", "back\\slash"}, `echo 'back\slash'`},
	} {
		if got := Quote(tc.argv); got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// TestQuoteSurvivesShell confirms the quoting round-trips through a real shell,
// which is what ssh hands the remote argv to.
func TestQuoteSurvivesShell(t *testing.T) {
	for _, arg := range []string{
		"plain", "with space", "it's", "$HOME", "a;b", "back\\slash", "*", "new\nline", "`cmd`", "",
	} {
		quoted := Quote([]string{"printf", "%s", arg})
		out, err := NewExec("", SudoNever, quiet()).Output(context.Background(), "sh", "-c", quoted)
		if err != nil {
			t.Fatalf("%q: %v", arg, err)
		}
		if out != arg {
			t.Errorf("shell round trip of %q gave %q", arg, out)
		}
	}
}

func TestOutputAndErrors(t *testing.T) {
	ctx := context.Background()
	e := NewExec("", SudoNever, quiet())

	out, err := e.Output(ctx, "printf", "%s", "hello")
	if err != nil || out != "hello" {
		t.Fatalf("Output = %q, %v", out, err)
	}

	_, err = e.Output(ctx, "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry stderr, got %v", err)
	}
	if !strings.Contains(err.Error(), "locally") {
		t.Errorf("error should say where it ran, got %v", err)
	}
}

func TestTest(t *testing.T) {
	ctx := context.Background()
	e := NewExec("", SudoNever, quiet())
	dir := t.TempDir()

	if ok, err := e.Test(ctx, "test", "-d", dir); err != nil || !ok {
		t.Fatalf("existing dir: %v %v", ok, err)
	}
	if ok, err := e.Test(ctx, "test", "-d", dir+"/absent"); err != nil || ok {
		t.Fatalf("absent dir: %v %v", ok, err)
	}
	// Exit status above 1 is a broken predicate, not a false one.
	if _, err := e.Test(ctx, "sh", "-c", "exit 4"); err == nil {
		t.Error("exit 4 should be an error")
	}
}

func TestPipe(t *testing.T) {
	ctx := context.Background()
	e := NewExec("", SudoNever, quiet())

	out, err := e.Pipe(ctx, []string{"printf", "%s", "streamed"}, e, []string{"cat"})
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	if out != "streamed" {
		t.Errorf("Pipe out = %q", out)
	}

	// A large payload must not deadlock: nothing buffers the stream in this
	// process, so the two children have to be running concurrently.
	out, err = e.Pipe(ctx, []string{"sh", "-c", "yes abcdefgh | head -c 4000000"}, e, []string{"wc", "-c"})
	if err != nil {
		t.Fatalf("large Pipe: %v", err)
	}
	if strings.TrimSpace(out) != "4000000" {
		t.Errorf("large Pipe out = %q", strings.TrimSpace(out))
	}
}

func TestPipeReportsSenderFirst(t *testing.T) {
	ctx := context.Background()
	e := NewExec("", SudoNever, quiet())
	// Sender fails; the receiver also fails on the truncated stream, but the
	// sender is the cause worth reporting.
	_, err := e.Pipe(ctx,
		[]string{"sh", "-c", "echo sender-broke >&2; exit 2"}, e,
		[]string{"sh", "-c", "cat; echo receiver-noise >&2; exit 5"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "sender-broke") {
		t.Errorf("want sender error, got %v", err)
	}
}

func TestPipeReportsReceiver(t *testing.T) {
	ctx := context.Background()
	e := NewExec("", SudoNever, quiet())
	_, err := e.Pipe(ctx, []string{"printf", "%s", "x"}, e, []string{"sh", "-c", "echo receiver-broke >&2; exit 1"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "receiver-broke") {
		t.Errorf("want receiver error, got %v", err)
	}
}

func TestRemoteWrapping(t *testing.T) {
	e := NewExec("nas", SudoAlways, quiet())
	got := Quote(e.wrap(true, []string{"btrfs", "send", "/pool/my snaps/x"}))
	want := `ssh nas 'sudo btrfs send '\''/pool/my snaps/x'\'''`
	if got != want {
		t.Errorf("wrap =\n %s\nwant\n %s", got, want)
	}

	local := NewExec("", SudoNever, quiet())
	if got := Quote(local.wrap(false, []string{"ls", "-1"})); got != "ls -1" {
		t.Errorf("local wrap = %q", got)
	}
}

// stubSSH puts a stand-in for ssh ahead of the real one on PATH. It records
// each argument it is given, one per line, so that a test can see exactly what
// crossed the boundary, and answers the "id -u" probe.
func stubSSH(t *testing.T, remoteUID string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\n" +
		"printf 'ARGC=%s\\n' \"$#\" >> " + log + "\n" +
		"for a in \"$@\"; do printf 'ARG=%s\\n' \"$a\" >> " + log + "; done\n" +
		"case \"$*\" in *'id -u'*) echo " + remoteUID + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestRemoteSudoIsResolvedOnceAndSentInside(t *testing.T) {
	ctx := context.Background()
	log := stubSSH(t, "1000") // the remote login is not root
	e := NewExec("nas", SudoAuto, quiet())

	for i := 0; i < 3; i++ {
		if _, err := e.Output(ctx, "btrfs", "subvolume", "show", "/pool/x"); err != nil {
			t.Fatal(err)
		}
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := string(recorded)

	// The host is asked who it is once, and sudo is confirmed once, however
	// many commands follow.
	if got := strings.Count(lines, "ARG=id -u"); got != 1 {
		t.Errorf("the remote was asked for its uid %d times, want 1:\n%s", got, lines)
	}
	if got := strings.Count(lines, "ARG=sudo true"); got != 1 {
		t.Errorf("the remote's sudo was confirmed %d times, want 1:\n%s", got, lines)
	}
	// sudo belongs inside the command ssh carries, not in front of ssh itself.
	if !strings.Contains(lines, "ARG=sudo btrfs subvolume show /pool/x") {
		t.Errorf("want the remote command under sudo, got:\n%s", lines)
	}
	// ssh is handed the host and one single argument, whatever the command.
	for _, argc := range strings.Split(strings.TrimSpace(lines), "\n") {
		if strings.HasPrefix(argc, "ARGC=") && argc != "ARGC=2" {
			t.Errorf("ssh should get two arguments, got %s", argc)
		}
	}
}

func TestRemoteRootNeedsNoSudo(t *testing.T) {
	log := stubSSH(t, "0") // the remote login is root
	e := NewExec("nas", SudoAuto, quiet())
	if _, err := e.Output(context.Background(), "btrfs", "subvolume", "show", "/pool/x"); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(recorded), "sudo") {
		t.Errorf("a root login should not be given sudo:\n%s", recorded)
	}
}

// stubSudo puts a stand-in for sudo ahead of the real one on PATH. It logs
// each invocation and then either runs the command, or fails the way sudo
// without a password does.
func stubSudo(t *testing.T, works bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n"
	if works {
		script += "exec \"$@\"\n"
	} else {
		script += "echo 'sudo: a password is required' >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// A sudo that does not work is reported as such; above all, Test must not read
// sudo's own exit status 1 as the predicate answering false.
func TestBrokenSudoIsAnErrorNotAFalsePredicate(t *testing.T) {
	stubSudo(t, false)
	e := NewExec("", SudoAlways, quiet())
	_, err := e.Test(context.Background(), "test", "-d", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("Test = %v", err)
	}
}

func TestSudoIsConfirmedOnce(t *testing.T) {
	log := stubSudo(t, true)
	e := NewExec("", SudoAlways, quiet())
	for i := 0; i < 3; i++ {
		if _, err := e.Output(context.Background(), "printf", "%s", "x"); err != nil {
			t.Fatal(err)
		}
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(recorded), "true\n"); got != 1 {
		t.Errorf("sudo was confirmed %d times, want 1:\n%s", got, recorded)
	}
}

func TestLocalSudoFollowsTheLogin(t *testing.T) {
	stubSudo(t, true)
	e := NewExec("", SudoAuto, quiet())
	got, err := e.useSudo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Geteuid() != 0; got != want {
		t.Errorf("useSudo = %v, want %v for uid %d", got, want, os.Geteuid())
	}

	for mode, want := range map[SudoMode]bool{SudoAlways: true, SudoNever: false} {
		got, err := NewExec("", mode, quiet()).useSudo(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("useSudo under %v = %v", mode, got)
		}
	}
}

func TestSudoMode(t *testing.T) {
	for _, name := range []string{"auto", "always", "never"} {
		m, err := ParseSudoMode(name)
		if err != nil {
			t.Fatalf("ParseSudoMode(%q): %v", name, err)
		}
		if m.String() != name {
			t.Errorf("round trip of %q gave %q", name, m.String())
		}
	}
	if _, err := ParseSudoMode("maybe"); err == nil {
		t.Error("expected rejection")
	}
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := NewFake("nas")
	f.Script([]string{"ls", "-1"}, Reply{Out: "a\nb\n"})
	f.Script([]string{"test", "-d", "/x"}, Reply{False: true})

	if out, err := f.Output(ctx, "ls", "-1"); err != nil || out != "a\nb\n" {
		t.Fatalf("Output = %q %v", out, err)
	}
	if ok, err := f.Test(ctx, "test", "-d", "/x"); err != nil || ok {
		t.Fatalf("Test = %v %v", ok, err)
	}
	if _, err := f.Output(ctx, "unscripted"); err == nil {
		t.Error("unscripted command should fail")
	}
	f.Script([]string{"tee", "--", "/x/f"}, Reply{})
	if err := f.Input(ctx, "data\n", "tee", "--", "/x/f"); err != nil {
		t.Fatal(err)
	}
	if got := f.Inputs["tee -- /x/f"]; got != "data\n" {
		t.Errorf("Inputs = %q", got)
	}
	if len(f.Calls) != 4 {
		t.Errorf("Calls = %v", f.Calls)
	}
}

func TestInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written")
	e := NewExec("", SudoNever, quiet())
	if err := e.Input(context.Background(), "line one\nline two\n", "tee", "--", path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "line one\nline two\n" {
		t.Errorf("wrote %q", got)
	}
}

// stubUnreachableSSH puts a stand-in for ssh on PATH that fails the way ssh
// does when it cannot reach its host.
func stubUnreachableSSH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'ssh: connect to host nas port 22: No route to host' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A host ssh cannot reach is told apart from a command that ran there and
// failed, whether the failure comes from resolving sudo or from the command.
func TestUnreachable(t *testing.T) {
	stubUnreachableSSH(t)
	ctx := context.Background()

	_, err := NewExec("nas", SudoAuto, quiet()).Output(ctx, "ls", "/pool")
	if !Unreachable(err) {
		t.Errorf("resolving sudo on an unreachable host: %v, want it unreachable", err)
	}
	_, err = NewExec("nas", SudoNever, quiet()).Test(ctx, "test", "-d", "/pool")
	if !Unreachable(err) {
		t.Errorf("a predicate on an unreachable host: %v, want it unreachable", err)
	}

	// A local command that happens to exit 255 did run.
	_, err = NewExec("", SudoNever, quiet()).Output(ctx, "sh", "-c", "exit 255")
	if err == nil || Unreachable(err) {
		t.Errorf("a local exit status of 255: %v, want a plain failure", err)
	}
}

// Once a local sudo is in use, its cached credentials are refreshed until the
// runner is closed, and never asked for interactively.
func TestLocalSudoIsKeptAlive(t *testing.T) {
	log := stubSudo(t, true)
	defer func(every time.Duration) { KeepAliveEvery = every }(KeepAliveEvery)
	KeepAliveEvery = 10 * time.Millisecond

	e := NewExec("", SudoAlways, quiet())
	if _, err := e.Output(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	refreshes := func() int {
		recorded, _ := os.ReadFile(log)
		return strings.Count(string(recorded), "-n -v\n")
	}
	deadline := time.Now().Add(5 * time.Second)
	for refreshes() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if refreshes() < 2 {
		t.Fatal("sudo's credentials were not refreshed")
	}

	e.Close()
	time.Sleep(50 * time.Millisecond) // a refresh already under way may finish
	after := refreshes()
	time.Sleep(100 * time.Millisecond)
	if refreshes() != after {
		t.Error("sudo was still being refreshed after Close")
	}
	e.Close() // closing twice is harmless
}

// A runner that never used sudo has nothing to keep alive.
func TestNoKeepAliveWithoutSudo(t *testing.T) {
	log := stubSudo(t, true)
	defer func(every time.Duration) { KeepAliveEvery = every }(KeepAliveEvery)
	KeepAliveEvery = 10 * time.Millisecond

	e := NewExec("", SudoNever, quiet())
	if _, err := e.Output(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	e.Close()
	if recorded, _ := os.ReadFile(log); len(recorded) != 0 {
		t.Errorf("sudo ran without being needed:\n%s", recorded)
	}
}
