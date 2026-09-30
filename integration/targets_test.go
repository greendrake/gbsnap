//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup disk comes and goes. While it is away its mount point is a bare,
// unmarked directory: gbsnap skips it rather than filling it, and pruning keeps
// the snapshot its record says the disk holds, so that the disk's next backup
// is incremental however much retention moved on meanwhile.
func TestDiskComesAndGoes(t *testing.T) {
	e := setup(t)
	e.newVolume("first")
	disk := filepath.Join(e.dst, "disk")      // where the disk is mounted
	away := filepath.Join(e.dst, "unplugged") // where it sits while away
	e.sudo("btrfs", "subvolume", "create", disk)
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + disk + "/home]\n" +
		"    retention:\n      pool: { min: 1 }\n")

	// Never marked, the disk is offline, and nothing is written to it.
	out := e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "offline") {
		t.Errorf("an unmarked target should be offline:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(disk, "home")); !os.IsNotExist(err) {
		t.Fatal("nothing should have been created on an unmarked target")
	}
	e.gbsnap(1, "-c", cfg, "sync", "-reach")

	e.gbsnap(0, "init", disk)
	e.gbsnap(0, "-c", cfg, "run")
	sent := e.latest(disk + "/home")

	// The disk is unplugged: its subvolume goes elsewhere, and a bare
	// directory is left where it was mounted.
	e.sudo("mv", disk, away)
	e.sudo("mkdir", disk)
	for _, content := range []string{"second", "third", "fourth"} {
		e.write(content)
		e.gbsnap(0, "-c", cfg, "run")
	}
	var kept bool
	for _, n := range e.snapshots(e.src + "/snap") {
		kept = kept || n == sent
	}
	if !kept {
		t.Fatalf("the snapshot the disk holds should have been kept; the pool holds %v", e.snapshots(e.src+"/snap"))
	}
	if entries, _ := os.ReadDir(disk); len(entries) != 0 {
		t.Fatalf("the bare mount point was written to: %v", entries)
	}
	out = e.gbsnap(0, "-c", cfg, "status")
	if !strings.Contains(out, "offline") || !strings.Contains(out, "last received "+sent) {
		t.Errorf("status should say the disk is offline and what it last received:\n%s", out)
	}

	// Plugged back in, the disk gets what it missed as a change.
	e.sudo("rmdir", disk)
	e.sudo("mv", away, disk)
	out = e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since "+sent) {
		t.Errorf("the disk's next backup should have been incremental:\n%s", out)
	}
	if got := e.read(filepath.Join(disk, "home", e.latest(disk+"/home"), "file")); got != "fourth" {
		t.Errorf("the disk holds %q", got)
	}
}

// A snapshot the machine has already let go of comes back from the backup, as
// a change from the original the machine still holds, which the backup's copy
// was received from.
func TestFetchPrunedSnapshotBack(t *testing.T) {
	e := setup(t)
	e.newVolume("oldest")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n" +
		"    retention:\n      pool: { min: 1 }\n      target: { min: 5 }\n")
	e.gbsnap(0, "init", e.dst)
	e.gbsnap(0, "init", e.src)
	e.gbsnap(0, "-c", cfg, "run")
	oldest := e.latest(e.src + "/snap")
	for _, content := range []string{"b", "c"} {
		e.write(content)
		e.gbsnap(0, "-c", cfg, "run")
	}
	for _, n := range e.snapshots(e.src + "/snap") {
		if n == oldest {
			t.Fatalf("the oldest snapshot should have been pruned on the machine: %v", e.snapshots(e.src+"/snap"))
		}
	}

	out := e.gbsnap(0, "sync", e.dst+"/backup", e.src+"/snap", "-snapshot", oldest)
	if !strings.Contains(out, "as a change since") {
		t.Errorf("the fetch should have built on a snapshot both sides hold:\n%s", out)
	}
	if got := e.read(filepath.Join(e.src, "snap", oldest, "file")); got != "oldest" {
		t.Errorf("the fetched snapshot holds %q", got)
	}
	// The pool and its backup still recognise each other afterwards.
	e.write("d")
	out = e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since") {
		t.Errorf("the next backup should still be incremental:\n%s", out)
	}
}

// Taking a tag off lets pruning have the snapshot, here and at the target.
func TestUntag(t *testing.T) {
	e := setup(t)
	e.newVolume("tagged")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n" +
		"    retention:\n      pool: { min: 1 }\n      target: { min: 1 }\n")
	e.gbsnap(0, "init", e.dst)
	e.gbsnap(0, "-c", cfg, "snap", "-tag", "keep")
	tagged := e.latest(e.src + "/snap")
	e.gbsnap(0, "-c", cfg, "sync")
	e.write("later")
	e.gbsnap(0, "-c", cfg, "run")

	e.gbsnap(0, "-c", cfg, "untag", "home", tagged)
	untagged := strings.TrimSuffix(tagged, "-keep")
	for _, pool := range []string{e.src + "/snap", e.dst + "/backup"} {
		names := strings.Join(e.snapshots(pool), " ")
		if strings.Contains(names, tagged) || !strings.Contains(names, untagged) {
			t.Errorf("%s holds %s, want %s untagged", pool, names, tagged)
		}
	}
	e.gbsnap(0, "-c", cfg, "prune")
	for _, pool := range []string{e.src + "/snap", e.dst + "/backup"} {
		if names := strings.Join(e.snapshots(pool), " "); strings.Contains(names, untagged) {
			t.Errorf("%s still holds %s once untagged: %s", pool, untagged, names)
		}
	}
}

// A pattern stands for the subvolumes in a directory, and for nothing else
// there: not plain directories, nor names starting with a dot.
func TestPatternFindsSubvolumes(t *testing.T) {
	e := setup(t)
	ws := filepath.Join(e.src, "ws")
	e.sudo("mkdir", ws)
	for _, sub := range []string{"one", "two", ".hidden"} {
		e.sudo("btrfs", "subvolume", "create", filepath.Join(ws, sub))
	}
	e.sudo("mkdir", filepath.Join(ws, "plain"))
	cfg := e.config("volumes:\n  ws:\n    subvolume: " + ws + "/*\n    pool: " + e.src + "/snap/*\n")

	out := e.gbsnap(0, "-c", cfg, "-n", "snap")
	for _, want := range []string{ws + "/one to ", ws + "/two to "} {
		if !strings.Contains(out, want) {
			t.Errorf("the pattern should have found %s:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"plain", ".hidden"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the pattern should not have found %s:\n%s", unwanted, out)
		}
	}
}

// Only btrfs can receive a snapshot, so no other filesystem is marked.
func TestInitRefusesOtherFilesystems(t *testing.T) {
	e := setup(t)
	if fs := strings.TrimSpace(e.sh("stat", "-f", "-c", "%T", e.dir)); fs == "btrfs" {
		t.Skip("the temporary directory is on btrfs")
	}
	out := e.gbsnap(1, "init", filepath.Join(e.dir, "elsewhere"))
	if !strings.Contains(out, "not btrfs") {
		t.Errorf("output:\n%s", out)
	}
}

// A volume a pattern stands for comes back from its backup after its subvolume
// and its pool have both gone, into a place marked for it.
func TestRestoreVolumeThatIsGone(t *testing.T) {
	e := setup(t)
	ws := filepath.Join(e.src, "ws")
	e.sudo("mkdir", ws)
	e.sudo("btrfs", "subvolume", "create", filepath.Join(ws, "one"))
	e.sudo("sh", "-c", "echo kept > "+filepath.Join(ws, "one", "file"))
	e.sudo("mkdir", filepath.Join(e.src, "snap"))
	e.gbsnap(0, "init", filepath.Join(e.src, "snap"))
	e.gbsnap(0, "init", e.dst)
	cfg := e.config("defaults:\n  targets: [" + e.dst + "]\nvolumes:\n  ws:\n    subvolume: " + ws +
		"/*\n    pool: " + e.src + "/snap/*\n")
	e.gbsnap(0, "-c", cfg, "run")
	name := e.latest(e.dst + "/one")

	e.sudo("btrfs", "subvolume", "delete", filepath.Join(e.src, "snap", "one", name))
	e.sudo("rm", "-rf", filepath.Join(e.src, "snap", "one"))
	e.sudo("btrfs", "subvolume", "delete", filepath.Join(ws, "one"))

	e.gbsnap(0, "-c", cfg, "restore", "one")
	if got := strings.TrimSpace(e.read(filepath.Join(e.src, "snap", "one", name, "file"))); got != "kept" {
		t.Errorf("the restored snapshot holds %q", got)
	}
}
