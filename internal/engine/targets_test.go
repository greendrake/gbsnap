package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/run"
)

// send scripts the pipe that sends a snapshot from the local pool to nas,
// as a change from parent unless parent is empty.
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

// A backup disk that is not mounted leaves a bare mount point: no pool there,
// and no marker. The target is offline, not an empty pool to fill.
func TestSyncSkipsOfflineTarget(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "/mnt/disk/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "mkdir")
	f.mustNotRun("", "btrfs send")
	if !strings.Contains(f.output(), "skipping /mnt/disk/home, which is offline") {
		t.Errorf("output = %q", f.output())
	}

	// Asked to reach it, the run fails instead.
	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")
	err := f.e.Sync(context.Background(), v, SyncOpts{Reach: true})
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("Sync with Reach = %v", err)
	}
}

// A host ssh cannot reach is offline too.
func TestSyncSkipsUnreachableHost(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("nas").Script([]string{"test", "-f", "/backup/" + pool.PlaceMarker},
		run.Reply{Err: &run.Error{Host: "nas", Unreachable: true, Err: context.DeadlineExceeded}})

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.output(), "nas cannot be reached") {
		t.Errorf("output = %q", f.output())
	}
}

// A pool made before places were marked keeps working where it is, known by
// its location.
func TestSyncToPoolInUnmarkedPlace(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.unmarked("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.copies("20260807T140000Z", "20260807T140100Z")
	f.send("20260807T140000Z", "20260807T140100Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	rs := f.saved("/pool/snap/home")
	if r, ok := rs.Get("nas:/backup/home"); !ok || r.Snapshot != "20260807T140100Z" {
		t.Errorf("records = %+v", rs)
	}
}

// Each send is recorded, keyed by the place's ID, with the newest snapshot the
// target now shares.
func TestSyncRecordsWhatTheTargetHolds(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.place("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.copies("20260807T140000Z", "20260807T140100Z")
	f.send("20260807T140000Z", "20260807T140100Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	r, ok := f.saved("/pool/snap/home").Get(placeID + "/home")
	if !ok || r.Snapshot != "20260807T140100Z" || r.Location != "nas:/backup/home" {
		t.Errorf("record = %+v, %v", r, ok)
	}
	if !r.Since.Equal(f.e.now()) {
		t.Errorf("since = %v", r.Since)
	}
}

// While a target is offline, pruning keeps the snapshot its record names,
// however far retention has moved on, so its next send can build on it.
func TestPruneKeepsWhatAnOfflineTargetNeeds(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "/mnt/disk/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.records("/pool/snap/home", pool.Record{
		Target: placeID + "/home", Location: "/mnt/disk/home", Snapshot: "20260807T140000Z"})
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")
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
		Target: placeID + "/home", Location: "nas:/backup/home", Snapshot: "20260807T140100Z"})
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
		Target: placeID + "/home", Location: "nas:/backup/home", Snapshot: "20260807T140000Z"})
	f.place("nas:/backup")
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
	f.place("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140200Z")
	f.copies("20260807T140000Z", "20260807T140200Z")
	f.send("20260807T140200Z", "20260807T140000Z")

	err := f.e.Sync(context.Background(), v, SyncOpts{Snapshot: "20260807T140000Z"})
	if err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "send -p /pool/snap/home/20260807T140200Z /pool/snap/home/20260807T140000Z")
	f.mustNotRun("", "/pool/snap/home/20260807T140100Z |")

	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	err = f.e.Sync(context.Background(), v, SyncOpts{Snapshot: "20260807T150000Z"})
	if err == nil || !strings.Contains(err.Error(), "holds no snapshot") {
		t.Errorf("fetching a snapshot the pool lacks = %v", err)
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
	f.place("nas:/backup")
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
// meet, so that they are paired again, but only once the UUID check has
// confirmed the copy is one.
func TestSyncFollowsTagChanges(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.place("nas:/backup")
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
	f.place("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140000Z-keep")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "src-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z-keep", shown{uuid: "dst-0", received: "other", readOnly: true})
	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err == nil {
		t.Error("expected the stray to be refused")
	}
	f.mustNotRun("nas", "mv")
}

func TestUntag(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home", "/mnt/disk/home")
	f.pool("/pool/snap/home", "20260807T140000Z-keep", "20260807T140100Z")
	f.records("/pool/snap/home", pool.Record{
		Target: placeID + "/home", Location: "/mnt/disk/home", Snapshot: "20260807T140000Z-keep"})
	f.host("").Script([]string{"mv", "-T", "--",
		"/pool/snap/home/20260807T140000Z-keep", "/pool/snap/home/20260807T140000Z"}, run.Reply{})
	f.place("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140000Z-keep")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: "src-0", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z-keep", shown{uuid: "dst-0", received: "src-0", readOnly: true})
	f.host("nas").Script([]string{"mv", "-T", "--",
		"/backup/home/20260807T140000Z-keep", "/backup/home/20260807T140000Z"}, run.Reply{})
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")

	if err := f.e.Untag(context.Background(), v, "20260807T140000Z-keep"); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "mv -T -- /pool/snap/home/20260807T140000Z-keep /pool/snap/home/20260807T140000Z")
	f.mustRun("nas", "mv -T -- /backup/home/20260807T140000Z-keep /backup/home/20260807T140000Z")
	if r, _ := f.saved("/pool/snap/home").Get(placeID + "/home"); r.Snapshot != "20260807T140000Z" {
		t.Errorf("the record should follow the rename, got %+v", r)
	}
	if !strings.Contains(f.output(), "/mnt/disk/home is offline") {
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
	f := newFixture(t, false)
	v := volume(t, "")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home",
		pool.Record{Target: placeID + "/home", Location: "/mnt/disk/home", Snapshot: "20260807T140000Z"},
		pool.Record{Target: "nas:/backup/home", Location: "nas:/backup/home", Snapshot: "20260807T140000Z"})

	// The place a target was in names it as well as its pool does.
	n, err := f.e.Forget(context.Background(), v, "/mnt/disk")
	if err != nil || n != 1 {
		t.Fatalf("Forget = %d, %v", n, err)
	}
	rs := f.saved("/pool/snap/home")
	if len(rs.List) != 1 || rs.List[0].Target != "nas:/backup/home" {
		t.Errorf("records = %+v", rs)
	}

	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home", pool.Record{Target: placeID + "/home", Location: "/mnt/disk/home"})
	if n, err := f.e.Forget(context.Background(), v, placeID); err != nil || n != 1 {
		t.Errorf("Forget by place ID = %d, %v", n, err)
	}
	if n, err := f.e.Forget(context.Background(), v, "/elsewhere"); err != nil || n != 0 {
		t.Errorf("Forget of a target never recorded = %d, %v", n, err)
	}
}

func TestInit(t *testing.T) {
	f := newFixture(t, false)
	marker := "/backup/" + pool.PlaceMarker
	f.host("nas").
		Script([]string{"test", "-f", marker}, run.Reply{False: true}).
		Script([]string{"mkdir", "-p", "--", "/backup"}, run.Reply{}).
		Script([]string{"stat", "-f", "-c", "%T", "--", "/backup"}, run.Reply{Out: "btrfs\n"}).
		Script([]string{"tee", "--", marker + ".new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", marker + ".new", marker}, run.Reply{})

	if err := f.e.Init(context.Background(), location.Location{Host: "nas", Path: "/backup"}, run.SudoAuto); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(f.host("nas").Inputs[run.Quote([]string{"tee", "--", marker + ".new"})])
	if len(id) != 32 || !strings.Contains(f.output(), id) {
		t.Errorf("marker holds %q; output %q", id, f.output())
	}

	// Anywhere but btrfs, a place could never hold a pool.
	f = newFixture(t, false)
	f.host("").
		Script([]string{"test", "-f", "/mnt/" + pool.PlaceMarker}, run.Reply{False: true}).
		Script([]string{"mkdir", "-p", "--", "/mnt"}, run.Reply{}).
		Script([]string{"stat", "-f", "-c", "%T", "--", "/mnt"}, run.Reply{Out: "ext2/ext3\n"})
	err := f.e.Init(context.Background(), location.Location{Path: "/mnt"}, run.SudoAuto)
	if err == nil || !strings.Contains(err.Error(), "not btrfs") {
		t.Errorf("Init on ext4 = %v", err)
	}

	// A place already marked keeps its ID.
	f = newFixture(t, false)
	f.place("/backup")
	if err := f.e.Init(context.Background(), location.Location{Path: "/backup"}, run.SudoAuto); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "tee")
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

// Status says when each target last received, which targets are offline, and
// which targets the pool still keeps a snapshot for though none is configured.
func TestStatusReportsRecords(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "/mnt/disk/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	since := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	f.records("/pool/snap/home",
		pool.Record{Target: placeID + "/home", Location: "/mnt/disk/home", Snapshot: "20260807T140000Z", Since: since},
		pool.Record{Target: "old:/b/home", Location: "old:/b/home", Snapshot: "20260807T140000Z", Since: since})
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")

	if err := f.e.Status(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	out := f.output()
	for _, want := range []string{"offline", "last received 20260807T140000Z", "old:/b/home", "gbsnap forget old:/b/home"} {
		if !strings.Contains(out, want) {
			t.Errorf("status should mention %q:\n%s", want, out)
		}
	}
}

// Restoring brings the pool back from the first target that can be reached and
// holds anything, sent from there into the pool.
func TestRestore(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "/mnt/disk/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140200Z")
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")
	f.place("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140000Z", "20260807T140200Z")
	f.place("/pool/snap")
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

	// With no target to be had, it says why.
	f = newFixture(t, false)
	f.pool("/pool/snap/home")
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")
	f.place("nas:/backup")
	f.pool("nas:/backup/home")
	err := f.e.Restore(context.Background(), v, "")
	if err == nil || !strings.Contains(err.Error(), "offline") || !strings.Contains(err.Error(), "holds no snapshots") {
		t.Errorf("Restore = %v", err)
	}
}
