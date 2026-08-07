//go:build integration

package integration

// Conversion from the predecessor's naming. This file goes with the command.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacySnapshot takes a read-only snapshot under a name the predecessor would
// have written, and copies it to the target under that same name, which is the
// state a pool pair is in before conversion.
func (e *env) legacySnapshot(name string) {
	e.t.Helper()
	e.sudo("mkdir", "-p", filepath.Join(e.src, "snap"), filepath.Join(e.dst, "backup"))
	e.sudo("btrfs", "subvolume", "snapshot", "-r",
		filepath.Join(e.src, "home"), filepath.Join(e.src, "snap", name))
	e.sudo("sh", "-c", "btrfs send "+filepath.Join(e.src, "snap", name)+
		" | btrfs receive "+filepath.Join(e.dst, "backup"))
}

// TestConvertKeepsTheIncrementalChain is the whole point of converting rather
// than starting afresh: after both sides are renamed, the snapshots they share
// are still recognised as shared, so the next copy is a difference and the
// history already at the target stays readable.
func TestConvertKeepsTheIncrementalChain(t *testing.T) {
	e := setup(t)
	e.newVolume("first")
	// Both of the predecessor's spellings, as a long-lived pool would hold.
	e.legacySnapshot("2023.06.06.01-pinned")
	e.write("second")
	e.legacySnapshot("2026-08-01.0001")
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n" +
		"    targets: [" + e.dst + "/backup]\n")

	// Until it is converted, the pool is refused outright, and the refusal
	// says what to do about it.
	out := e.gbsnap(1, "-c", cfg, "run")
	if !strings.Contains(out, "gbsnap convert") {
		t.Errorf("the refusal should point at the conversion:\n%s", out)
	}

	// A dry run reports the renames and makes none of them.
	out = e.gbsnap(0, "-c", cfg, "-n", "convert")
	if !strings.Contains(out, "would rename") {
		t.Errorf("dry run:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.src, "snap", "2026-08-01.0001")); err != nil {
		t.Errorf("a dry run must leave the old name in place: %v", err)
	}

	e.gbsnap(0, "-c", cfg, "convert")

	// Both sides carry the new names, the tag among them.
	for _, pool := range []string{e.src + "/snap", e.dst + "/backup"} {
		got := strings.Join(e.snapshots(pool), " ")
		want := "20230606T000000Z-pinned 20260801T000000Z"
		if got != want {
			t.Errorf("pool %s holds %q, want %q", pool, got, want)
		}
	}

	// The snapshot the target already held is still readable under its new
	// name: renaming moved no data.
	if got := e.read(filepath.Join(e.dst, "backup", "20230606T000000Z-pinned", "file")); got != "first" {
		t.Errorf("target holds %q, want %q", got, "first")
	}

	// And now the ordinary cycle picks up where the predecessor left off,
	// sending a difference rather than everything again.
	e.write("third")
	out = e.gbsnap(0, "-c", cfg, "run")
	if !strings.Contains(out, "as a change since 20260801T000000Z") {
		t.Errorf("the copy should have been incremental on the converted parent:\n%s", out)
	}
	newest := e.latest(e.src + "/snap")
	if got := e.read(filepath.Join(e.dst, "backup", newest, "file")); got != "third" {
		t.Errorf("target holds %q, want %q", got, "third")
	}

	// Converting again has nothing left to do.
	out = e.gbsnap(0, "-c", cfg, "-v", "convert")
	if !strings.Contains(out, "already in this tool's naming") {
		t.Errorf("a second conversion:\n%s", out)
	}
}

// A pool that cannot be converted cleanly is left exactly as it was.
func TestConvertRefusesAndChangesNothing(t *testing.T) {
	e := setup(t)
	e.newVolume("content")
	e.legacySnapshot("2026-08-01.0001")
	// Something neither naming accounts for.
	e.sudo("touch", filepath.Join(e.src, "snap", "notes.txt"))
	cfg := e.config("volumes:\n  home:\n" +
		"    subvolume: " + e.src + "/home\n" +
		"    pool: " + e.src + "/snap\n")

	out := e.gbsnap(1, "-c", cfg, "convert")
	if !strings.Contains(out, "notes.txt") {
		t.Errorf("the refusal should name the entry:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.src, "snap", "2026-08-01.0001")); err != nil {
		t.Errorf("the pool should be untouched: %v", err)
	}
}
