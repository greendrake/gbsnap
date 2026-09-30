package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/run"
)

// send scripts the pipe that sends a snapshot from the local pool to nas, as a
// change from parent unless parent is empty.
func (f *fixture) send(parent, name string) {
	argv := []string{"btrfs", "send"}
	if parent != "" {
		argv = append(argv, "-p", "/pool/snap/home/"+parent)
	}
	argv = append(argv, "/pool/snap/home/"+name)
	f.host("").Replies[run.PipeKey(argv, run.NewFake("nas"), []string{"btrfs", "receive", "/backup/home"})] = run.Reply{}
}

// copies scripts snapshots held on both sides, the target's received from the
// pool's.
func (f *fixture) copies(names ...string) {
	for _, n := range names {
		f.subvolume("/pool/snap/home/"+n, shown{uuid: "src-" + n, readOnly: true})
		f.subvolume("nas:/backup/home/"+n, shown{uuid: "dst-" + n, received: "src-" + n, readOnly: true})
	}
}

// A target is taken as written: a pool that is not there is created, given an
// ID, and sent everything, whatever is or is not mounted there.
func TestSyncCreatesTargetPool(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "/mnt/disk/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.missingPool("/mnt/disk/home")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "src-0", readOnly: true})
	f.subvolume("/mnt/disk/home/20260807T140000Z", shown{uuid: "dst-0", received: "src-0", readOnly: true})
	f.host("").Replies[run.PipeKey(
		[]string{"btrfs", "send", "/pool/snap/home/20260807T140000Z"},
		run.NewFake(""),
		[]string{"btrfs", "receive", "/mnt/disk/home"})] = run.Reply{}

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "mkdir -p -- /mnt/disk/home")
	id := strings.TrimSpace(f.host("").Inputs[run.Quote([]string{"tee", "--", "/mnt/disk/home/.gbsnap-id.new"})])
	if !pool.IsID(id) {
		t.Fatalf("the new pool got the ID %q", id)
	}
	if r, ok := f.saved("/pool/snap/home").Get(id); !ok || r.Snapshot != "20260807T140000Z" {
		t.Errorf("the send should be recorded under the new pool's ID: %+v", f.saved("/pool/snap/home"))
	}
}

// A host ssh cannot reach is skipped, saying what ssh said, since ssh fails
// alike for a host that is off and one that turns the login away; asked to
// reach it, the run fails instead.
func TestSyncSkipsUnreachableHost(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.unreachable("nas:/backup/home")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.output(), "nas cannot be reached") || !strings.Contains(f.output(), "No route to host") {
		t.Errorf("output = %q", f.output())
	}
	f.mustNotRun("", "btrfs send")

	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.unreachable("nas:/backup/home")
	err := f.e.Sync(context.Background(), v, SyncOpts{Reach: true})
	if err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Errorf("Sync with Reach = %v", err)
	}
}

// A pool without an ID, made by hand or by an earlier version, is given one
// when next sent to, and the send recorded under it.
func TestSyncGivesPoolAnID(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.unidentified("nas:/backup/home")
	f.copies("20260807T140000Z", "20260807T140100Z")
	f.send("20260807T140000Z", "20260807T140100Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(f.host("nas").Inputs[run.Quote([]string{"tee", "--", "/backup/home/.gbsnap-id.new"})])
	if r, ok := f.saved("/pool/snap/home").Get(id); !pool.IsID(id) || !ok || r.Snapshot != "20260807T140100Z" {
		t.Errorf("ID %q, records %+v", id, f.saved("/pool/snap/home"))
	}
}

// Each send is recorded, keyed by the target pool's ID, with the newest
// snapshot the target now shares.
func TestSyncRecordsWhatTheTargetHolds(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.copies("20260807T140000Z", "20260807T140100Z")
	f.send("20260807T140000Z", "20260807T140100Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	r, ok := f.saved("/pool/snap/home").Get(poolID)
	if !ok || r.Snapshot != "20260807T140100Z" || r.Location != "nas:/backup/home" {
		t.Errorf("record = %+v, %v", r, ok)
	}
	if !r.Since.Equal(f.e.now()) {
		t.Errorf("since = %v", r.Since)
	}
	f.mustNotRun("nas", "tee") // the pool had its ID already
}

// While a target cannot be reached, pruning keeps the snapshot its record
// names, however far retention has moved on, so its next send can build on it.
func TestPruneKeepsWhatAnUnreachableTargetNeeds(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.records("/pool/snap/home", pool.Record{
		Target: poolID, Location: "nas:/backup/home", Snapshot: "20260807T140000Z"})
	f.unreachable("nas:/backup/home")
	f.host("").Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/20260807T140100Z"}, run.Reply{})

	if err := f.e.Prune(context.Background(), v, PruneOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "subvolume delete /pool/snap/home/20260807T140100Z")
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140000Z")
}

// Two disks taking turns at one mount point are two targets: the one there now
// is reached and pruned with, and the other's record still keeps what it
// needs.
func TestPruneKeepsWhatTheDiskAwayNeeds(t *testing.T) {
	other := "fedcba9876543210fedcba9876543210"
	f := newFixture(t, false)
	v := volume(t, "", "/mnt/disk/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z", "20260807T140300Z")
	f.records("/pool/snap/home",
		pool.Record{Target: other, Location: "/mnt/disk/home", Snapshot: "20260807T140000Z"},
		pool.Record{Target: poolID, Location: "/mnt/disk/home", Snapshot: "20260807T140200Z"})
	f.pool("/mnt/disk/home", "20260807T140200Z")
	f.host("").Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/20260807T140100Z"}, run.Reply{})

	if err := f.e.Prune(context.Background(), v, PruneOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "subvolume delete /pool/snap/home/20260807T140100Z")
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140000Z")
}

// A local pruning reaches no target at all, and goes by the records alone.
func TestPruneLocal(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.records("/pool/snap/home", pool.Record{
		Target: poolID, Location: "nas:/backup/home", Snapshot: "20260807T140100Z"})
	f.host("").Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/20260807T140000Z"}, run.Reply{})

	if err := f.e.Prune(context.Background(), v, PruneOpts{Local: true}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "subvolume delete /pool/snap/home/20260807T140000Z")
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140100Z")
	if calls := f.host("nas").Calls; len(calls) != 0 {
		t.Errorf("a local pruning reached the target: %v", calls)
	}
}

// A reachable target that no longer shares anything with the pool has nothing
// left to keep for it, so its record goes.
func TestPruneDropsRecordOfTargetSharingNothing(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.records("/pool/snap/home", pool.Record{
		Target: poolID, Location: "nas:/backup/home", Snapshot: "20260807T140000Z"})
	f.pool("nas:/backup/home")
	f.host("").Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/20260807T140000Z"}, run.Reply{})

	if err := f.e.Prune(context.Background(), v, PruneOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "subvolume delete /pool/snap/home/20260807T140000Z")
	if rs := f.saved("/pool/snap/home"); len(rs.List) != 0 {
		t.Errorf("records = %+v", rs)
	}
}

// A named snapshot older than anything the target shares is sent on its own,
// as a change from the nearest snapshot both hold: here the one after it.
func TestFetchOlderSnapshot(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.pool("nas:/backup/home", "20260807T140200Z")
	f.copies("20260807T140000Z", "20260807T140200Z")
	f.send("20260807T140200Z", "20260807T140000Z")

	err := f.e.Sync(context.Background(), v, SyncOpts{Snapshot: "20260807T140000Z"})
	if err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "send -p /pool/snap/home/20260807T140200Z /pool/snap/home/20260807T140000Z")
	f.mustNotRun("", "/pool/snap/home/20260807T140100Z |")

	for _, held := range [][]string{{"20260807T140000Z"}, nil} {
		f = newFixture(t, false)
		f.pool("/pool/snap/home", held...)
		err = f.e.Sync(context.Background(), v, SyncOpts{Snapshot: "20260807T150000Z"})
		if err == nil || !strings.Contains(err.Error(), "holds no snapshot") {
			t.Errorf("fetching a snapshot a pool holding %v lacks = %v", held, err)
		}
	}
}

func TestNearestShared(t *testing.T) {
	names := func(ss ...string) []pool.Name {
		var ns []pool.Name
		for _, s := range ss {
			n, err := pool.ParseName(s)
			if err != nil {
				t.Fatal(err)
			}
			ns = append(ns, n)
		}
		return ns
	}
	src := names("20260807T140000Z", "20260807T140100Z", "20260807T140200Z", "20260807T140300Z")
	for _, tc := range []struct {
		dst  []pool.Name
		want string
	}{
		{names("20260807T140000Z", "20260807T140300Z"), "20260807T140000Z"}, // the newest older one
		{names("20260807T140300Z"), "20260807T140300Z"},                     // else the oldest newer one
		{names("20260807T140100Z", "20260807T140300Z"), "20260807T140100Z"},
		{nil, ""},
	} {
		n, ok := nearestShared(src, tc.dst, src[2])
		got := ""
		if ok {
			got = n.String()
		}
		if got != tc.want {
			t.Errorf("nearestShared with %v = %q, want %q", tc.dst, got, tc.want)
		}
	}
}

// Sent back the other way, as a restore is, the destination holds the very
// snapshot the pool's copy was received from, which is a parent to build on.
func TestRestoreBuildsOnTheOriginal(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.pool("nas:/backup/home", "20260807T140000Z")
	// The pool is the backup here: its copies were received from the
	// destination's originals.
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "copy-0", received: "orig-0", readOnly: true})
	f.subvolume("/pool/snap/home/20260807T140100Z", shown{uuid: "copy-1", received: "orig-1", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{uuid: "orig-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140100Z", shown{uuid: "back-1", received: "orig-1", readOnly: true})
	f.send("20260807T140000Z", "20260807T140100Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "send -p /pool/snap/home/20260807T140000Z /pool/snap/home/20260807T140100Z")
}

// A tag taken off in the pool is taken off the target's copy when the two next
// meet, but only once the UUID check has confirmed the copy is one.
func TestSyncFollowsTagChanges(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.pool("nas:/backup/home", "20260807T140000Z-keep")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "src-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z-keep", shown{uuid: "dst-0", received: "src-0", readOnly: true})
	f.host("nas").Script([]string{"mv", "-T", "--",
		"/backup/home/20260807T140000Z-keep", "/backup/home/20260807T140000Z"}, run.Reply{})

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("nas", "mv -T -- /backup/home/20260807T140000Z-keep /backup/home/20260807T140000Z")
	f.mustNotRun("", "btrfs send") // paired again, so up to date

	// A stray that is not a copy keeps its name, and the sync stops there.
	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.pool("nas:/backup/home", "20260807T140000Z-keep")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "src-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z-keep", shown{uuid: "dst-0", received: "other", readOnly: true})
	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err == nil {
		t.Error("expected the stray to be refused")
	}
	f.mustNotRun("nas", "mv")
}

// Sent the other way, as a restore is, the pool is the backup, whose names are
// the stale ones: a tag taken off on the machine is neither put back nor sent
// back as a second copy, since the two are one snapshot whatever their tags.
func TestRestoreLeavesUntaggedAlone(t *testing.T) {
	back := func() (*fixture, *config.Volume) {
		f := newFixture(t, false)
		v := config.AdHoc(parse(t, "nas:/backup/home"), []location.Location{parse(t, "/pool/snap/home")}, 3, run.SudoAuto)
		f.pool("nas:/backup/home", "20260807T140000Z", "20260807T140100Z-keep")
		f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
		return f, v
	}
	f, v := back()
	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "mv")
	f.mustNotRun("nas", "btrfs send")

	// Asked for by either name, the snapshot is already there.
	for _, name := range []string{"20260807T140100Z", "20260807T140100Z-keep"} {
		f, v := back()
		if err := f.e.Sync(context.Background(), v, SyncOpts{Snapshot: name}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		f.mustNotRun("nas", "btrfs send")
	}
}

// A parent the target holds under another tag is built on where the target
// holds it.
func TestBuildOnParentUnderAnotherTag(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z-keep", "20260807T140100Z")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.subvolume("/pool/snap/home/20260807T140000Z-keep", shown{uuid: "src-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{uuid: "dst-0", received: "src-0", readOnly: true})
	f.subvolume("/pool/snap/home/20260807T140100Z", shown{uuid: "src-1", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140100Z", shown{uuid: "dst-1", received: "src-1", readOnly: true})
	f.send("20260807T140000Z-keep", "20260807T140100Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "send -p /pool/snap/home/20260807T140000Z-keep /pool/snap/home/20260807T140100Z")
}

func TestUntag(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home", "usb:/disk/home")
	f.pool("/pool/snap/home", "20260807T140000Z-keep", "20260807T140100Z")
	f.records("/pool/snap/home", pool.Record{
		Target: poolID, Location: "usb:/disk/home", Snapshot: "20260807T140000Z-keep"})
	f.host("").Script([]string{"mv", "-T", "--",
		"/pool/snap/home/20260807T140000Z-keep", "/pool/snap/home/20260807T140000Z"}, run.Reply{})
	f.pool("nas:/backup/home", "20260807T140000Z-keep")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "src-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z-keep", shown{uuid: "dst-0", received: "src-0", readOnly: true})
	f.host("nas").Script([]string{"mv", "-T", "--",
		"/backup/home/20260807T140000Z-keep", "/backup/home/20260807T140000Z"}, run.Reply{})
	f.unreachable("usb:/disk/home")

	if err := f.e.Untag(context.Background(), v, "20260807T140000Z-keep"); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "mv -T -- /pool/snap/home/20260807T140000Z-keep /pool/snap/home/20260807T140000Z")
	f.mustRun("nas", "mv -T -- /backup/home/20260807T140000Z-keep /backup/home/20260807T140000Z")
	if r, _ := f.saved("/pool/snap/home").Get(poolID); r.Snapshot != "20260807T140000Z" {
		t.Errorf("the record should follow the rename, got %+v", r)
	}
	if !strings.Contains(f.output(), "usb:/disk/home is offline") {
		t.Errorf("output = %q", f.output())
	}

	for name, snapshot := range map[string]string{"untagged": "20260807T140100Z", "absent": "20260807T150000Z-x"} {
		f := newFixture(t, false)
		f.pool("/pool/snap/home", "20260807T140000Z-keep", "20260807T140100Z")
		if err := f.e.Untag(context.Background(), volume(t, ""), snapshot); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestForget(t *testing.T) {
	other := "fedcba9876543210fedcba9876543210"
	v := volume(t, "")

	f := newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home",
		pool.Record{Target: poolID, Location: "/mnt/disk/home", Snapshot: "20260807T140000Z"},
		pool.Record{Target: other, Location: "nas:/backup/home", Snapshot: "20260807T140000Z"})
	// The directory a target was in names it as well as its pool does.
	n, err := f.e.Forget(context.Background(), v, "/mnt/disk")
	if err != nil || n != 1 {
		t.Fatalf("Forget = %d, %v", n, err)
	}
	if rs := f.saved("/pool/snap/home"); len(rs.List) != 1 || rs.List[0].Target != other {
		t.Errorf("records = %+v", rs)
	}

	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home", pool.Record{Target: poolID, Location: "/mnt/disk/home"})
	if n, err := f.e.Forget(context.Background(), v, poolID); err != nil || n != 1 {
		t.Errorf("Forget by ID = %d, %v", n, err)
	}
	if n, err := f.e.Forget(context.Background(), v, "/elsewhere"); err != nil || n != 0 {
		t.Errorf("Forget of a target never recorded = %d, %v", n, err)
	}

	// Two disks took turns at one mount point: where they were names both, so
	// only an ID will do.
	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home",
		pool.Record{Target: poolID, Location: "/mnt/disk/home"},
		pool.Record{Target: other, Location: "/mnt/disk/home"})
	if _, err := f.e.Forget(context.Background(), v, "/mnt/disk"); err == nil || !strings.Contains(err.Error(), other) {
		t.Errorf("Forget of a place two targets shared = %v", err)
	}
	if n, err := f.e.Forget(context.Background(), v, other); err != nil || n != 1 {
		t.Errorf("Forget by ID = %d, %v", n, err)
	}
	if rs := f.saved("/pool/snap/home"); len(rs.List) != 1 || rs.List[0].Target != poolID {
		t.Errorf("records = %+v", rs)
	}
}

func TestSubvolumes(t *testing.T) {
	f := newFixture(t, false)
	f.host("").
		Script([]string{"test", "-d", "/ws"}, run.Reply{}).
		Script([]string{"find", "/ws", "-mindepth", "1", "-maxdepth", "1", "-type", "d", "-inum", "256",
			"!", "-name", ".*", "-printf", "%f\n"}, run.Reply{Out: "wild\ncupla\n"}).
		Script([]string{"test", "-d", "/absent"}, run.Reply{False: true})

	got, err := f.e.Subvolumes(context.Background(), location.Location{Path: "/ws"}, run.SudoAuto)
	if err != nil || strings.Join(got, " ") != "wild cupla" {
		t.Errorf("Subvolumes = %q, %v", got, err)
	}
	got, err = f.e.Subvolumes(context.Background(), location.Location{Path: "/absent"}, run.SudoAuto)
	if err != nil || len(got) != 0 {
		t.Errorf("Subvolumes of a missing directory = %q, %v", got, err)
	}
}

// Status says when each target last received, which targets are offline (with
// the record's ID, which forget takes), and which targets the pool still keeps
// a snapshot for though none is configured.
func TestStatusReportsRecords(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	since := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	other := "fedcba9876543210fedcba9876543210"
	f.records("/pool/snap/home",
		pool.Record{Target: poolID, Location: "nas:/backup/home", Snapshot: "20260807T140000Z", Since: since},
		pool.Record{Target: other, Location: "old:/b/home", Snapshot: "20260807T140000Z", Since: since})
	f.unreachable("nas:/backup/home")

	if err := f.e.Status(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	out := f.output()
	for _, want := range []string{"offline", "last received 20260807T140000Z", "record " + poolID,
		"old:/b/home", "gbsnap forget " + other} {
		if !strings.Contains(out, want) {
			t.Errorf("status should mention %q:\n%s", want, out)
		}
	}
}

// Restoring brings the pool back from the first target that can be reached and
// holds anything, sent from there into the pool, and writes nothing at the
// source.
func TestRestore(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "usb:/disk/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140200Z")
	f.unreachable("usb:/disk/home")
	f.pool("nas:/backup/home", "20260807T140000Z", "20260807T140200Z")
	f.subvolume("nas:/backup/home/20260807T140200Z", shown{uuid: "copy-2", received: "orig-2", readOnly: true})
	f.subvolume("/pool/snap/home/20260807T140200Z", shown{uuid: "orig-2", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{uuid: "copy-0", received: "orig-0", readOnly: true})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "back-0", received: "orig-0", readOnly: true})
	f.host("nas").Replies[run.PipeKey(
		[]string{"btrfs", "send", "-p", "/backup/home/20260807T140200Z", "/backup/home/20260807T140000Z"},
		run.NewFake(""),
		[]string{"btrfs", "receive", "/pool/snap/home"})] = run.Reply{}

	if err := f.e.Restore(context.Background(), v, "20260807T140000Z"); err != nil {
		t.Fatal(err)
	}
	f.mustRun("nas", "send -p /backup/home/20260807T140200Z /backup/home/20260807T140000Z | [local] btrfs receive /pool/snap/home")
	f.mustNotRun("nas", "tee")
	f.mustNotRun("", "tee")

	// A named snapshot comes from the first target that holds it.
	f = newFixture(t, false)
	v2 := volume(t, "/home", "nas:/backup/home", "/mnt/disk/home")
	f.pool("/pool/snap/home", "20260807T140200Z")
	f.pool("nas:/backup/home", "20260807T140200Z")
	f.pool("/mnt/disk/home", "20260807T140000Z", "20260807T140200Z")
	f.subvolume("/mnt/disk/home/20260807T140200Z", shown{uuid: "copy-2", received: "orig-2", readOnly: true})
	f.subvolume("/pool/snap/home/20260807T140200Z", shown{uuid: "orig-2", readOnly: true})
	f.subvolume("/mnt/disk/home/20260807T140000Z", shown{uuid: "copy-0", received: "orig-0", readOnly: true})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "back-0", received: "orig-0", readOnly: true})
	f.host("").Replies[run.PipeKey(
		[]string{"btrfs", "send", "-p", "/mnt/disk/home/20260807T140200Z", "/mnt/disk/home/20260807T140000Z"},
		run.NewFake(""),
		[]string{"btrfs", "receive", "/pool/snap/home"})] = run.Reply{}
	if err := f.e.Restore(context.Background(), v2, "20260807T140000Z"); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "send -p /mnt/disk/home/20260807T140200Z /mnt/disk/home/20260807T140000Z")

	// With no target to be had, it says why.
	f = newFixture(t, false)
	f.pool("/pool/snap/home")
	f.unreachable("usb:/disk/home")
	f.pool("nas:/backup/home")
	err := f.e.Restore(context.Background(), v, "")
	if err == nil || !strings.Contains(err.Error(), "cannot be reached") || !strings.Contains(err.Error(), "holds no snapshots") {
		t.Errorf("Restore = %v", err)
	}
}

// A pool gbsnap 0.2.0 sent to has no ID and a record keyed another way: the
// record is carried over to the ID the pool gets, rather than left to linger.
func TestRecordFrom020IsCarriedOver(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home",
		pool.Record{Target: "0123456789abcdef0123456789abcdef/home", Location: "nas:/backup/home", Snapshot: "20260807T140000Z"})
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.unidentified("nas:/backup/home")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(f.host("nas").Inputs[run.Quote([]string{"tee", "--", "/backup/home/.gbsnap-id.new"})])
	rs := f.saved("/pool/snap/home")
	if len(rs.List) != 1 || rs.List[0].Target != id || rs.List[0].Snapshot != "20260807T140000Z" {
		t.Errorf("ID %s, records %+v", id, rs)
	}
}

// A pool made at the location while the one a 0.2.0 record is about is away
// holds nothing of it, and the record stays to keep what that one needs.
func TestRecordFrom020StaysForThePoolAway(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "/mnt/disk/home")
	v.Retention.Pool.Min = 1
	earlier := "0123456789abcdef0123456789abcdef/home"
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.records("/pool/snap/home",
		pool.Record{Target: earlier, Location: "/mnt/disk/home", Snapshot: "20260807T140000Z"})
	f.missingPool("/mnt/disk/home")
	for _, n := range []string{"20260807T140000Z", "20260807T140100Z", "20260807T140200Z"} {
		f.subvolume("/pool/snap/home/"+n, shown{uuid: "src-" + n, readOnly: true})
		f.subvolume("/mnt/disk/home/"+n, shown{uuid: "dst-" + n, received: "src-" + n, readOnly: true})
	}
	for _, step := range [][2]string{{"", "20260807T140000Z"}, {"20260807T140000Z", "20260807T140100Z"},
		{"20260807T140100Z", "20260807T140200Z"}} {
		argv := []string{"btrfs", "send"}
		if step[0] != "" {
			argv = append(argv, "-p", "/pool/snap/home/"+step[0])
		}
		argv = append(argv, "/pool/snap/home/"+step[1])
		f.host("").Replies[run.PipeKey(argv, run.NewFake(""), []string{"btrfs", "receive", "/mnt/disk/home"})] = run.Reply{}
	}

	if err := f.e.Run(context.Background(), v, false); err != nil {
		t.Fatal(err)
	}
	rs := f.saved("/pool/snap/home")
	if r, ok := rs.Get(earlier); !ok || r.Snapshot != "20260807T140000Z" {
		t.Errorf("the earlier record should stay as it was: %+v", rs)
	}
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140000Z")
}
