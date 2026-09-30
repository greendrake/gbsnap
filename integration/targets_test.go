//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

// Two disks take turns at one mount point. Each is a target of its own, known
// by the ID gbsnap gives its pool, so while one is away pruning keeps what its
// next backup builds on, and it gets a change, not everything, when it is back.
func TestDisksTakingTurns(t *testing.T) {
	e := setup(t)
	e.newVolume("first")
	disk := filepath.Join(e.dst, "disk") // where either disk is mounted
	shelf := filepath.Join(e.dst, "shelf")
	e.sudo("mkdir", shelf)
	e.sudo("btrfs", "subvolume", "create", disk) // disk A, mounted
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + disk + "/home]\n" +
		"    retention:\n      pool: { min: 1 }\n")

	e.gbsnap(0, "-c", cfg, "run")
	sentToA := e.latest(disk + "/home")
	if id := strings.TrimSpace(e.read(filepath.Join(disk, "home", ".gbsnap-id"))); len(id) != 32 {
		t.Fatalf("the new pool's ID is %q", id)
	}

	// Disk A goes on the shelf and disk B, new, takes its place.
	e.sudo("mv", disk, filepath.Join(shelf, "a"))
	e.sudo("btrfs", "subvolume", "create", disk)
	for _, content := range []string{"second", "third"} {
		e.write(content)
		e.gbsnap(0, "-c", cfg, "run")
	}
	var kept bool
	for _, n := range e.snapshots(e.src + "/snap") {
		kept = kept || n == sentToA
	}
	if !kept {
		t.Fatalf("the snapshot disk A holds should have been kept; the pool holds %v", e.snapshots(e.src+"/snap"))
	}

	// Disk A is back, and gets what it missed as a change.
	e.sudo("mv", disk, filepath.Join(shelf, "b"))
	e.sudo("mv", filepath.Join(shelf, "a"), disk)
	out := e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since "+sentToA) {
		t.Errorf("disk A's next backup should have been incremental:\n%s", out)
	}
	if got := e.read(filepath.Join(disk, "home", e.latest(disk+"/home"), "file")); got != "third" {
		t.Errorf("disk A holds %q", got)
	}
	out = e.gbsnap(0, "-c", cfg, "status")
	if strings.Count(out, "last received") != 2 {
		t.Errorf("status should show both disks' records:\n%s", out)
	}
}

// Snapshots taken and pruned locally keep what the backup's next send builds
// on, without reaching the backup at all.
func TestLocalPruningKeepsWhatTheNextSendNeeds(t *testing.T) {
	e := setup(t)
	e.newVolume("first")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n" +
		"    retention:\n      pool: { min: 1 }\n")
	e.gbsnap(0, "-c", cfg, "run")
	sent := e.latest(e.dst + "/backup")
	for _, content := range []string{"second", "third", "fourth"} {
		e.write(content)
		e.gbsnap(0, "-c", cfg, "snap")
		e.gbsnap(0, "-c", cfg, "prune", "-local")
	}
	out := e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since "+sent) {
		t.Errorf("the next backup should have been incremental:\n%s", out)
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

// A volume a pattern stands for comes back from its backup after its subvolume
// and its pool have both gone.
func TestRestoreVolumeThatIsGone(t *testing.T) {
	e := setup(t)
	ws := filepath.Join(e.src, "ws")
	e.sudo("mkdir", ws)
	e.sudo("btrfs", "subvolume", "create", filepath.Join(ws, "one"))
	e.sudo("sh", "-c", "echo kept > "+filepath.Join(ws, "one", "file"))
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
