package engine

// Goes with convert.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/run"
)

// legacy scripts a pool holding predecessor snapshots, each a read-only
// subvolume, and the renames that convert them.
func (f *fixture) legacy(loc string, entries ...string) {
	f.pool(loc, entries...)
	for _, entry := range entries {
		f.subvolume(loc+"/"+entry, shown{uuid: "uuid-" + entry, readOnly: true})
	}
}

func (f *fixture) scriptRename(loc, from, to string) {
	l := parse(f.t, loc)
	f.host(l.Host).Script([]string{"mv", "-T", "--", l.Path + "/" + from, l.Path + "/" + to}, run.Reply{})
}

func TestConvertRenamesPoolAndTargets(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.legacy("/pool/snap/home", "2026-07-11.0001-keep", "2026-08-02.0001", "2026-08-02.0002")
	f.legacy("nas:/backup/home", "2026-07-11.0001-keep", "2026-08-02.0001")
	for _, r := range [][2]string{
		{"2026-07-11.0001-keep", "20260711T000000Z-keep"},
		{"2026-08-02.0001", "20260802T000000Z"},
		{"2026-08-02.0002", "20260802T000000Z.2"},
	} {
		f.scriptRename("/pool/snap/home", r[0], r[1])
	}
	f.scriptRename("nas:/backup/home", "2026-07-11.0001-keep", "20260711T000000Z-keep")
	f.scriptRename("nas:/backup/home", "2026-08-02.0001", "20260802T000000Z")

	if err := f.e.Convert(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	// A target is converted alongside its pool, or the two would no longer
	// recognise the snapshots they share.
	f.mustRun("", "mv -T -- /pool/snap/home/2026-08-02.0002 /pool/snap/home/20260802T000000Z.2")
	f.mustRun("nas", "mv -T -- /backup/home/2026-08-02.0001 /backup/home/20260802T000000Z")
	f.mustRun("nas", "mv -T -- /backup/home/2026-07-11.0001-keep /backup/home/20260711T000000Z-keep")
}

// The predecessor spelled the date two ways over its life, and a pool may hold
// both.
func TestConvertBothPredecessorSpellings(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.legacy("/pool/snap/home", "2023.06.06.01-pinned", "2023.12.31.02", "2026-08-02.0001")
	for _, r := range [][2]string{
		{"2023.06.06.01-pinned", "20230606T000000Z-pinned"},
		{"2023.12.31.02", "20231231T000000Z.2"},
		{"2026-08-02.0001", "20260802T000000Z"},
	} {
		f.scriptRename("/pool/snap/home", r[0], r[1])
	}

	if err := f.e.Convert(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "mv -T -- /pool/snap/home/2023.06.06.01-pinned /pool/snap/home/20230606T000000Z-pinned")
	f.mustRun("", "mv -T -- /pool/snap/home/2023.12.31.02 /pool/snap/home/20231231T000000Z.2")
	f.mustRun("", "mv -T -- /pool/snap/home/2026-08-02.0001 /pool/snap/home/20260802T000000Z")
}

// One date and sequence written both ways is two snapshots that would become
// one name. Letting either win would lose the other, so the pool is refused.
func TestConvertRefusesSpellingCollision(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.legacy("/pool/snap/home", "2023-06-06.0001", "2023.06.06.01")

	err := f.e.Convert(context.Background(), v)
	if err == nil || !strings.Contains(err.Error(), "would both become 20230606T000000Z") {
		t.Fatalf("Convert = %v", err)
	}
	f.mustNotRun("", "mv")
}

// Names already in this tool's form are left where they are, so a conversion
// that was interrupted finishes by being run again.
func TestConvertLeavesConvertedNamesAlone(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260711T000000Z-keep", "2026-08-02.0001")
	f.subvolume("/pool/snap/home/2026-08-02.0001", shown{uuid: "u", readOnly: true})
	f.scriptRename("/pool/snap/home", "2026-08-02.0001", "20260802T000000Z")

	if err := f.e.Convert(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "mv -T -- /pool/snap/home/2026-08-02.0001")
	f.mustNotRun("", "20260711T000000Z-keep /pool")
}

func TestConvertNothingToDo(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260802T000000Z")

	if err := f.e.Convert(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "mv")
}

// A pool holding something neither naming accounts for is left alone entirely.
func TestConvertRefusesUnknownEntries(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "2026-08-02.0001", "notes.txt")

	err := f.e.Convert(context.Background(), v)
	if err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("Convert = %v", err)
	}
	f.mustNotRun("", "mv")
}

// Nothing is renamed at all when any one rename would land on a name the pool
// already holds, so a pool that cannot be converted cleanly is left as it was.
func TestConvertRefusesCollision(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "2026-08-02.0001", "20260802T000000Z")
	f.subvolume("/pool/snap/home/2026-08-02.0001", shown{uuid: "u", readOnly: true})

	err := f.e.Convert(context.Background(), v)
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("Convert = %v", err)
	}
	f.mustNotRun("", "mv")
}

// Something that is not a read-only snapshot could never be sent, so it is
// refused rather than adopted into the pool.
func TestConvertRefusesWritableSubvolume(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "2026-08-02.0001")
	f.subvolume("/pool/snap/home/2026-08-02.0001", shown{uuid: "u"})

	err := f.e.Convert(context.Background(), v)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("Convert = %v", err)
	}
	f.mustNotRun("", "mv")
}

func TestConvertDryRunRenamesNothing(t *testing.T) {
	f := newFixture(t, true)
	v := volume(t, "/home")
	f.legacy("/pool/snap/home", "2026-08-02.0001")

	if err := f.e.Convert(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "mv")
	if !strings.Contains(f.output(), "would rename /pool/snap/home/2026-08-02.0001 to 20260802T000000Z") {
		t.Errorf("output = %q", f.output())
	}
}

func TestConvertAdHocRefusesMissingPool(t *testing.T) {
	f := newFixture(t, false)
	f.missingPool("/pool/absent")

	err := f.e.Convert(context.Background(), config.AdHoc(parse(t, "/pool/absent"), nil, 1, run.SudoNever))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Convert = %v", err)
	}
}
