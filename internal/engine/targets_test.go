package engine

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/config"
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
		run.Reply{Err: &run.Error{Host: "nas", Unreachable: true, Err: context.DeadlineExceeded,
			Stderr: "user@nas: Permission denied (publickey)."}})

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	// ssh fails alike for a host that is off and one that turns the login
	// away, so what ssh said is passed on.
	if !strings.Contains(f.output(), "nas cannot be reached") || !strings.Contains(f.output(), "Permission denied") {
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

// Sent the other way, as a restore is, the pool is the backup, whose names are
// the stale ones: a tag taken off on the machine is neither put back nor sent
// back as a second copy, since the two are one snapshot whatever their tags.
func TestRestoreLeavesUntaggedAlone(t *testing.T) {
	back := func() (*fixture, *config.Volume) {
		f := newFixture(t, false)
		v := config.AdHoc(parse(t, "nas:/backup/home"), []location.Location{parse(t, "/pool/snap/home")}, 3, run.SudoAuto)
		f.pool("nas:/backup/home", "20260807T140000Z", "20260807T140100Z-keep")
		f.place("/pool/snap")
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
	f.place("nas:/backup")
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

	// Two disks took turns at one mount point: where they were names both, so
	// only a key will do.
	other := "fedcba9876543210fedcba9876543210"
	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home",
		pool.Record{Target: placeID + "/home", Location: "/mnt/disk/home"},
		pool.Record{Target: other + "/home", Location: "/mnt/disk/home"})
	if _, err := f.e.Forget(context.Background(), v, "/mnt/disk"); err == nil || !strings.Contains(err.Error(), other) {
		t.Errorf("Forget of a place two targets shared = %v", err)
	}
	if n, err := f.e.Forget(context.Background(), v, other+"/home"); err != nil || n != 1 {
		t.Errorf("Forget by key = %d, %v", n, err)
	}
	if rs := f.saved("/pool/snap/home"); len(rs.List) != 1 || rs.List[0].Target != placeID+"/home" {
		t.Errorf("records = %+v", rs)
	}

	// A path is never taken for the start of a key.
	f = newFixture(t, false)
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.records("/pool/snap/home", pool.Record{Target: "/mnt/disk/home", Location: "/mnt/disk/home"})
	if n, err := f.e.Forget(context.Background(), v, "/mnt"); err != nil || n != 0 {
		t.Errorf("Forget /mnt = %d, %v", n, err)
	}
}

// placeProbe scripts what init reads of a directory before marking it.
func (f *fixture) placeProbe(host, dir, fs, devs, entries string) {
	marker := path.Join(dir, pool.PlaceMarker)
	f.host(host).
		Script([]string{"test", "-f", marker}, run.Reply{False: true}).
		Script([]string{"test", "-d", dir}, run.Reply{}).
		Script([]string{"stat", "-f", "-c", "%T", "--", dir}, run.Reply{Out: fs + "\n"}).
		Script([]string{"stat", "-c", "%d", "--", dir, path.Join(dir, "..")}, run.Reply{Out: devs}).
		Script([]string{"ls", "-A", "--", dir}, run.Reply{Out: entries}).
		Script([]string{"tee", "--", marker + ".new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", marker + ".new", marker}, run.Reply{})
}

func TestInit(t *testing.T) {
	// A disk's mount point, a device of its own: marked, with a new ID.
	f := newFixture(t, false)
	f.placeProbe("nas", "/backup", "btrfs", "41\n40\n", "")
	if err := f.e.Init(context.Background(), location.Location{Host: "nas", Path: "/backup"}, run.SudoAuto, false); err != nil {
		t.Fatal(err)
	}
	marker := "/backup/" + pool.PlaceMarker
	id := strings.TrimSpace(f.host("nas").Inputs[run.Quote([]string{"tee", "--", marker + ".new"})])
	if len(id) != 32 || !strings.Contains(f.output(), id) {
		t.Errorf("marker holds %q; output %q", id, f.output())
	}
	f.mustNotRun("nas", "mkdir")

	// An empty directory on the filesystem holding it is what a disk that is
	// not mounted leaves: refused, unless forced.
	f = newFixture(t, false)
	f.placeProbe("", "/mnt/disk", "btrfs", "40\n40\n", "")
	err := f.e.Init(context.Background(), location.Location{Path: "/mnt/disk"}, run.SudoAuto, false)
	if err == nil || !strings.Contains(err.Error(), "mount it first") {
		t.Errorf("Init of a bare mount point = %v", err)
	}
	f.mustNotRun("", "tee")
	if err := f.e.Init(context.Background(), location.Location{Path: "/mnt/disk"}, run.SudoAuto, true); err != nil {
		t.Errorf("Init -force = %v", err)
	}
	f.mustRun("", "tee")

	// A directory with something in it is no bare mount point.
	f = newFixture(t, false)
	f.placeProbe("", "/srv/backup", "btrfs", "40\n40\n", "home\n")
	if err := f.e.Init(context.Background(), location.Location{Path: "/srv/backup"}, run.SudoAuto, false); err != nil {
		t.Errorf("Init of a directory already in use = %v", err)
	}

	// Anywhere but btrfs, a place could never hold a pool.
	f = newFixture(t, false)
	f.placeProbe("", "/mnt", "ext2/ext3", "40\n41\n", "")
	err = f.e.Init(context.Background(), location.Location{Path: "/mnt"}, run.SudoAuto, false)
	if err == nil || !strings.Contains(err.Error(), "not btrfs") {
		t.Errorf("Init on ext4 = %v", err)
	}

	// Nor is any directory made.
	f = newFixture(t, false)
	f.host("").
		Script([]string{"test", "-f", "/absent/" + pool.PlaceMarker}, run.Reply{False: true}).
		Script([]string{"test", "-d", "/absent"}, run.Reply{False: true})
	err = f.e.Init(context.Background(), location.Location{Path: "/absent"}, run.SudoAuto, false)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("Init of a missing directory = %v", err)
	}

	// A place already marked keeps its ID.
	f = newFixture(t, false)
	f.place("/backup")
	if err := f.e.Init(context.Background(), location.Location{Path: "/backup"}, run.SudoAuto, false); err != nil {
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
	for _, want := range []string{"offline", "last received 20260807T140000Z", "record " + placeID + "/home",
		"old:/b/home", "gbsnap forget old:/b/home"} {
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
	// The source of a send, here the backup, is never written to.
	f.mustNotRun("nas", "tee")
	f.mustNotRun("", "tee")

	// A named snapshot comes from the first target that holds it.
	f = newFixture(t, false)
	v2 := volume(t, "/home", "nas:/backup/home", "/mnt/disk/home")
	f.pool("/pool/snap/home", "20260807T140200Z")
	f.place("nas:/backup")
	f.pool("nas:/backup/home", "20260807T140200Z")
	f.place("/mnt/disk")
	f.pool("/mnt/disk/home", "20260807T140000Z", "20260807T140200Z")
	f.place("/pool/snap")
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
	f.unmarked("/mnt/disk")
	f.missingPool("/mnt/disk/home")
	f.place("nas:/backup")
	f.pool("nas:/backup/home")
	err := f.e.Restore(context.Background(), v, "")
	if err == nil || !strings.Contains(err.Error(), "offline") || !strings.Contains(err.Error(), "holds no snapshots") {
		t.Errorf("Restore = %v", err)
	}
}
