//go:build integration

// Package integration exercises gbsnap against real btrfs filesystems.
//
// The tests need root, or a passwordless sudo, and the btrfs tools. They skip
// cleanly where those are missing. Run them with:
//
//	go test -tags integration ./integration/
package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greendrake/gbsnap/internal/cli"
)

// env is a pair of btrfs filesystems: one holding the live subvolume and its
// pool, the other standing in for a backup host.
type env struct {
	t   *testing.T
	dir string
	src string // mount point of the source filesystem
	dst string // mount point of the destination filesystem
}

func setup(t *testing.T) *env {
	t.Helper()
	for _, tool := range []string{"btrfs", "mkfs.btrfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if os.Geteuid() != 0 {
		if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
			t.Skip("needs root or a passwordless sudo")
		}
	}

	dir, err := os.MkdirTemp("", "gbsnap-integration-")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, src: filepath.Join(dir, "src"), dst: filepath.Join(dir, "dst")}
	t.Cleanup(func() {
		exec.Command("sudo", "umount", e.src).Run()
		exec.Command("sudo", "umount", e.dst).Run()
		exec.Command("sudo", "rm", "-rf", dir).Run()
	})

	for _, mount := range []string{e.src, e.dst} {
		image := mount + ".img"
		if err := os.MkdirAll(mount, 0o755); err != nil {
			t.Fatal(err)
		}
		e.sh("truncate", "-s", "512M", image)
		e.sh("mkfs.btrfs", "-q", image)
		e.sudo("mount", "-o", "loop", image, mount)
	}
	return e
}

// sh runs a command that needs no privilege.
func (e *env) sh(name string, args ...string) string {
	e.t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// sudo runs a command as root, which the test may already be.
func (e *env) sudo(name string, args ...string) string {
	e.t.Helper()
	if os.Geteuid() != 0 {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	return e.sh(name, args...)
}

// gbsnap runs the tool as the command line would, failing the test on an
// unexpected exit code.
func (e *env) gbsnap(wantCode int, args ...string) string {
	e.t.Helper()
	var out bytes.Buffer
	code := cli.Main(args, &out, &out)
	if code != wantCode {
		e.t.Fatalf("gbsnap %s: exit %d, want %d\n%s", strings.Join(args, " "), code, wantCode, out.String())
	}
	return out.String()
}

// config writes a configuration file and returns its path.
func (e *env) config(body string) string {
	e.t.Helper()
	path := filepath.Join(e.dir, "gbsnap.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// snapshots lists a pool.
func (e *env) snapshots(pool string) []string {
	e.t.Helper()
	entries, err := os.ReadDir(pool)
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func (e *env) latest(pool string) string {
	e.t.Helper()
	names := e.snapshots(pool)
	if len(names) == 0 {
		e.t.Fatalf("pool %s is empty", pool)
	}
	return names[len(names)-1]
}

// read returns the contents of a file inside a snapshot, which root owns.
func (e *env) read(path string) string {
	e.t.Helper()
	return e.sudo("cat", path)
}

// newVolume creates the live subvolume with a file in it.
func (e *env) newVolume(content string) {
	e.t.Helper()
	e.sudo("btrfs", "subvolume", "create", filepath.Join(e.src, "home"))
	e.sudo("chown", "-R", currentUser(e.t), filepath.Join(e.src, "home"))
	e.write(content)
}

func (e *env) write(content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.src, "home", "file"), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func currentUser(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestCycle walks a volume through the states a real one goes through: a first
// full copy, runs where nothing has changed, a change to metadata alone, a
// change to the data, and finally retention letting go of the oldest.
func TestCycle(t *testing.T) {
	e := setup(t)
	e.newVolume("first")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n" +
		"    retention:\n      pool: { min: 2 }\n      target: { min: 2 }\n")

	// A first run creates both pools and copies the snapshot in full, the
	// target's in the place marked for it.
	e.gbsnap(0, "init", e.dst)
	e.gbsnap(0, "-c", cfg, "run")
	if got := len(e.snapshots(e.src + "/snap")); got != 1 {
		t.Fatalf("pool holds %d snapshots, want 1", got)
	}
	name := e.latest(e.src + "/snap")
	if got := e.read(filepath.Join(e.dst, "backup", name, "file")); got != "first" {
		t.Fatalf("target holds %q, want %q", got, "first")
	}

	// Nothing has changed, so the generations agree and gbsnap neither snapshots
	// nor writes a probe.
	out := e.gbsnap(0, "-c", cfg, "-v", "run")
	if got := len(e.snapshots(e.src + "/snap")); got != 1 {
		t.Fatalf("an unchanged volume gained a snapshot: %v", e.snapshots(e.src+"/snap"))
	}
	if strings.Contains(out, ".gbsnap-probe") {
		t.Errorf("an unchanged volume should be settled by generations alone:\n%s", out)
	}
	if !strings.Contains(out, "unchanged since "+name) {
		t.Errorf("output:\n%s", out)
	}

	// Reading a file moves the generation without changing anything, so the
	// difference has to be examined before gbsnap can tell.
	e.read(filepath.Join(e.src, "home", "file"))
	out = e.gbsnap(0, "-c", cfg, "-v", "run")
	if got := len(e.snapshots(e.src + "/snap")); got != 1 {
		t.Fatalf("reading a file gained a snapshot: %v", e.snapshots(e.src+"/snap"))
	}
	if !strings.Contains(out, ".gbsnap-probe") {
		t.Errorf("a moved generation should have been examined:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.src, "snap", ".gbsnap-probe")); !os.IsNotExist(err) {
		t.Error("the probe snapshot should have been cleared away")
	}

	// A change to permissions alone is a change worth keeping.
	e.sudo("chmod", "600", filepath.Join(e.src, "home", "file"))
	e.gbsnap(0, "-c", cfg, "run")
	if got := len(e.snapshots(e.src + "/snap")); got != 2 {
		t.Fatalf("a permission change was not snapshotted: %v", e.snapshots(e.src+"/snap"))
	}

	// A change to the data is sent as a difference from the snapshot already at
	// the target.
	e.write("second")
	out = e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since") {
		t.Errorf("the second copy should have been incremental:\n%s", out)
	}
	names := e.snapshots(e.src + "/snap")
	if len(names) != 3 {
		t.Fatalf("pool holds %v, want three snapshots", names)
	}
	newest := names[len(names)-1]
	if got := e.read(filepath.Join(e.dst, "backup", newest, "file")); got != "second" {
		t.Fatalf("target holds %q, want %q", got, "second")
	}
	// The first copy is still readable, which is the point of keeping history.
	if got := e.read(filepath.Join(e.dst, "backup", name, "file")); got != "first" {
		t.Fatalf("the oldest snapshot at the target holds %q, want %q", got, "first")
	}

	// A fourth snapshot puts the oldest beyond the retention of two.
	e.write("third")
	e.gbsnap(0, "-c", cfg, "run")
	if got := e.snapshots(e.src + "/snap"); len(got) != 3 {
		// Two by policy, plus the parent the target still needs.
		t.Fatalf("pool holds %v, want three snapshots after pruning", got)
	}
	for _, n := range e.snapshots(e.src + "/snap") {
		if n == name {
			t.Errorf("the oldest snapshot %s should have been pruned", name)
		}
	}
}

// A tagged snapshot stays however far retention advances past it.
func TestTagProtectsFromPruning(t *testing.T) {
	e := setup(t)
	e.newVolume("content")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    retention:\n      pool: { min: 1 }\n")

	e.gbsnap(0, "-c", cfg, "snap", "-tag", "keep")
	tagged := e.latest(e.src + "/snap")
	if !strings.HasSuffix(tagged, "-keep") {
		t.Fatalf("snapshot %q should carry the tag", tagged)
	}
	for i := 0; i < 3; i++ {
		e.write(strings.Repeat("x", i+1))
		e.gbsnap(0, "-c", cfg, "run")
	}

	names := e.snapshots(e.src + "/snap")
	var found bool
	for _, n := range names {
		if n == tagged {
			found = true
		}
	}
	if !found {
		t.Errorf("the tagged snapshot is gone; pool holds %v", names)
	}
	if len(names) != 2 {
		t.Errorf("pool holds %v, want the tagged snapshot and the newest one", names)
	}
}

// A destination snapshot that gbsnap did not put there must never be built on.
func TestRefusesDivergedTarget(t *testing.T) {
	e := setup(t)
	e.newVolume("content")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n")

	e.gbsnap(0, "init", e.dst)
	e.gbsnap(0, "-c", cfg, "run")
	name := e.latest(e.src + "/snap")

	// Replace the target's copy with a snapshot of something else, keeping the
	// name. Only the UUID trail gives it away.
	e.sudo("btrfs", "subvolume", "delete", filepath.Join(e.dst, "backup", name))
	e.sudo("btrfs", "subvolume", "create", filepath.Join(e.dst, "impostor"))
	e.sudo("btrfs", "subvolume", "snapshot", "-r",
		filepath.Join(e.dst, "impostor"), filepath.Join(e.dst, "backup", name))

	e.write("changed")
	out := e.gbsnap(1, "-c", cfg, "run")
	if !strings.Contains(out, "diverged") {
		t.Errorf("gbsnap should have refused the impostor:\n%s", out)
	}
	// Refusing must come before anything is let go of.
	if got := len(e.snapshots(e.src + "/snap")); got != 2 {
		t.Errorf("pool holds %d snapshots; the refusal should not have pruned", got)
	}
}

// Replication reads the same in reverse, which is what a restore is.
func TestRestoreIsSyncReversed(t *testing.T) {
	e := setup(t)
	e.newVolume("original")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n")
	e.gbsnap(0, "init", e.dst)
	e.gbsnap(0, "-c", cfg, "run")
	name := e.latest(e.src + "/snap")

	// The pool is lost, and the backup is sent back into a new one, in a place
	// marked for it.
	e.sudo("btrfs", "subvolume", "delete", filepath.Join(e.src, "snap", name))
	e.gbsnap(0, "init", e.src)
	e.gbsnap(0, "sync", e.dst+"/backup", e.src+"/restored")

	if got := e.read(filepath.Join(e.src, "restored", name, "file")); got != "original" {
		t.Fatalf("restored copy holds %q, want %q", got, "original")
	}
}

// The same work over ssh, where the tool has to quote a whole command for a
// remote shell to take apart again.
func TestOverSSH(t *testing.T) {
	e := setup(t)
	sshFixture(t, e.dir)
	e.newVolume("over ssh")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [127.0.0.1:" + e.dst + "/backup]\n")

	// The place is marked over ssh too.
	e.gbsnap(0, "init", "127.0.0.1:"+e.dst)
	e.gbsnap(0, "-c", cfg, "run")
	name := e.latest(e.src + "/snap")
	if got := e.read(filepath.Join(e.dst, "backup", name, "file")); got != "over ssh" {
		t.Fatalf("target holds %q", got)
	}

	e.write("changed over ssh")
	out := e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since") {
		t.Errorf("the second copy should have been incremental:\n%s", out)
	}
	newest := e.latest(e.src + "/snap")
	if got := e.read(filepath.Join(e.dst, "backup", newest, "file")); got != "changed over ssh" {
		t.Fatalf("target holds %q", got)
	}
}

// A remote path that a shell would take apart, sent through ssh, which hands
// gbsnap's command to a shell to take apart.
func TestAwkwardRemotePath(t *testing.T) {
	e := setup(t)
	sshFixture(t, e.dir)
	e.newVolume("awkward")
	awkward := e.dst + "/it's a backup; really"
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [\"127.0.0.1:" + awkward + "\"]\n")

	e.gbsnap(0, "init", "127.0.0.1:"+e.dst)
	e.gbsnap(0, "-c", cfg, "run")
	name := e.latest(e.src + "/snap")
	if got := e.read(filepath.Join(awkward, name, "file")); got != "awkward" {
		t.Fatalf("target holds %q, want %q", got, "awkward")
	}

	e.write("still awkward")
	e.gbsnap(0, "-c", cfg, "run")
	newest := e.latest(e.src + "/snap")
	if got := e.read(filepath.Join(awkward, newest, "file")); got != "still awkward" {
		t.Fatalf("target holds %q, want %q", got, "still awkward")
	}
}
