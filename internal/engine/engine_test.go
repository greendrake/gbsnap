package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/pool"
	"github.com/greendrake/gbsnap/internal/retention"
	"github.com/greendrake/gbsnap/internal/run"
	"github.com/greendrake/gbsnap/internal/ui"
)

// The clock every test snapshot is named after.
const nowStamp = "20260807T143205Z"

type fixture struct {
	t     *testing.T
	e     *Engine
	out   *bytes.Buffer
	hosts map[string]*run.Fake
	// modes records each runner the engine asked for, as host|sudo.
	modes []string
}

func newFixture(t *testing.T, dryRun bool) *fixture {
	t.Helper()
	out := &bytes.Buffer{}
	f := &fixture{t: t, out: out, hosts: map[string]*run.Fake{}}
	f.e = New(&ui.Printer{Out: out, Err: out}, dryRun)
	f.e.now = func() time.Time { return time.Date(2026, 8, 7, 14, 32, 5, 0, time.UTC) }
	f.e.newRunner = func(host string, sudo run.SudoMode) run.Runner {
		f.modes = append(f.modes, host+"|"+sudo.String())
		return f.host(host)
	}
	return f
}

func (f *fixture) host(name string) *run.Fake {
	if r, ok := f.hosts[name]; ok {
		return r
	}
	r := run.NewFake(name)
	f.hosts[name] = r
	return r
}

// poolID is the ID of every pool the tests script as having one.
const poolID = "0123456789abcdef0123456789abcdef"

// pool scripts a pool directory holding the named snapshots, with an ID, and
// with no records of any target yet, able to have them written.
func (f *fixture) pool(loc string, entries ...string) {
	l := parse(f.t, loc)
	records := path.Join(l.Path, pool.RecordsName)
	id := path.Join(l.Path, pool.IDName)
	f.host(l.Host).
		Script([]string{"test", "-d", l.Path}, run.Reply{}).
		Script([]string{"ls", "-1", "--", l.Path}, run.Reply{Out: strings.Join(entries, "\n") + "\n"}).
		Script([]string{"test", "-f", records}, run.Reply{False: true}).
		Script([]string{"tee", "--", records + ".new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", records + ".new", records}, run.Reply{}).
		Script([]string{"test", "-f", id}, run.Reply{}).
		Script([]string{"cat", "--", id}, run.Reply{Out: poolID + "\n"})
}

// unidentified scripts a pool that has no ID yet, and can be given one.
func (f *fixture) unidentified(loc string) {
	l := parse(f.t, loc)
	id := path.Join(l.Path, pool.IDName)
	f.host(l.Host).
		Script([]string{"test", "-f", id}, run.Reply{False: true}).
		Script([]string{"tee", "--", id + ".new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", id + ".new", id}, run.Reply{})
}

// unreachable scripts a pool on a host ssh cannot reach.
func (f *fixture) unreachable(loc string) {
	l := parse(f.t, loc)
	f.host(l.Host).Script([]string{"test", "-d", l.Path}, run.Reply{Err: &run.Error{
		Host: l.Host, Unreachable: true, Err: errors.New("exit status 255"),
		Stderr: "ssh: connect to host " + l.Host + " port 22: No route to host"}})
}

// records scripts what a pool knows of its targets.
func (f *fixture) records(loc string, rs ...pool.Record) {
	l := parse(f.t, loc)
	data, err := json.Marshal(pool.Records{List: rs})
	if err != nil {
		f.t.Fatal(err)
	}
	records := path.Join(l.Path, pool.RecordsName)
	f.host(l.Host).
		Script([]string{"test", "-f", records}, run.Reply{}).
		Script([]string{"cat", "--", records}, run.Reply{Out: string(data)})
}

// saved reads back the records gbsnap last wrote for a pool.
func (f *fixture) saved(loc string) *pool.Records {
	f.t.Helper()
	l := parse(f.t, loc)
	data, ok := f.host(l.Host).Inputs[run.Quote([]string{"tee", "--", path.Join(l.Path, pool.RecordsName) + ".new"})]
	if !ok {
		f.t.Fatalf("no records were written for %s", loc)
	}
	rs := &pool.Records{}
	if err := json.Unmarshal([]byte(data), rs); err != nil {
		f.t.Fatal(err)
	}
	return rs
}

// missingPool scripts a pool directory that is not there, and so holds no
// records either, though they, and its ID, can be written once it is created.
func (f *fixture) missingPool(loc string) {
	l := parse(f.t, loc)
	records := path.Join(l.Path, pool.RecordsName)
	f.host(l.Host).
		Script([]string{"test", "-d", l.Path}, run.Reply{False: true}).
		Script([]string{"test", "-f", records}, run.Reply{False: true}).
		Script([]string{"tee", "--", records + ".new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", records + ".new", records}, run.Reply{}).
		Script([]string{"mkdir", "-p", "--", l.Path}, run.Reply{})
	f.unidentified(loc)
}

// subvolume scripts what "btrfs subvolume show" reports for a path.
func (f *fixture) subvolume(loc string, s shown) {
	l := parse(f.t, loc)
	f.host(l.Host).Script([]string{"btrfs", "subvolume", "show", l.Path}, run.Reply{Out: s.render()})
}

// shown is the handful of fields gbsnap reads out of a subvolume.
type shown struct {
	uuid, parent, received string
	generation             uint64
	readOnly               bool
}

func (s shown) render() string {
	dash := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	flags := "-"
	if s.readOnly {
		flags = "readonly"
	}
	return fmt.Sprintf("subvol\n\tName: \tsubvol\n\tUUID: \t%s\n\tParent UUID: \t%s\n"+
		"\tReceived UUID: \t%s\n\tGeneration: \t%d\n\tFlags: \t%s\n\tSnapshot(s):\n",
		s.uuid, dash(s.parent), dash(s.received), s.generation, flags)
}

func parse(t *testing.T, s string) location.Location {
	t.Helper()
	l, err := location.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// ran reports whether a host was asked to run a command matching the substring.
func (f *fixture) ran(host, substring string) bool {
	for _, c := range f.host(host).Calls {
		if strings.Contains(c, substring) {
			return true
		}
	}
	return false
}

func (f *fixture) mustRun(host, substring string) {
	f.t.Helper()
	if !f.ran(host, substring) {
		f.t.Errorf("expected %q to run on host %q; it ran:\n  %s",
			substring, host, strings.Join(f.host(host).Calls, "\n  "))
	}
}

func (f *fixture) mustNotRun(host, substring string) {
	f.t.Helper()
	if f.ran(host, substring) {
		f.t.Errorf("did not expect %q to run on host %q", substring, host)
	}
}

func (f *fixture) output() string { return f.out.String() }

// volume builds a volume wired to local paths under /pool.
func volume(t *testing.T, subvolume string, targets ...string) *config.Volume {
	v := &config.Volume{
		Name:      "home",
		Pool:      parse(t, "/pool/snap/home"),
		Retention: config.Retention{Pool: retention.Policy{Min: 3}, Target: retention.Policy{Min: 3}},
	}
	if subvolume != "" {
		sub := parse(t, subvolume)
		v.Subvolume = &sub
	}
	for _, target := range targets {
		v.Targets = append(v.Targets, parse(t, target))
	}
	return v
}

const (
	liveUUID = "11111111-1111-1111-1111-111111111111"
	snapUUID = "22222222-2222-2222-2222-222222222222"
)

// snapshotPipe is the probe: a metadata-only send of the throwaway snapshot,
// decoded on the invoking host.
func probeKey(parent string) string {
	return run.PipeKey(
		[]string{"btrfs", "send", "--no-data", "-p", parent, "/pool/snap/home/.gbsnap-probe"},
		run.NewFake(""),
		[]string{"btrfs", "receive", "--dump"})
}

const dumpUnchanged = "snapshot ./x uuid=a transid=2\nutimes ./x/f atime=1 mtime=2 ctime=3\n"
const dumpChanged = dumpUnchanged + "update_extent ./x/f offset=0 len=6\n"

func TestSnapUnchangedByGeneration(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	f.subvolume("/home", shown{uuid: liveUUID, generation: 42})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: liveUUID, generation: 42, readOnly: true})

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "subvolume snapshot")
	// Reading generations must not have cost a probe, which would have written
	// to the pool.
	f.mustNotRun("", ".gbsnap-probe")
	if !strings.Contains(f.output(), "unchanged since 20260807T140000Z") {
		t.Errorf("output = %q", f.output())
	}
}

func TestSnapUnchangedByProbe(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	// The generation moved, which on its own says nothing: reading a file moves
	// it too.
	f.subvolume("/home", shown{uuid: liveUUID, generation: 43})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: liveUUID, generation: 42, readOnly: true})
	f.host("").
		Script([]string{"test", "-e", "/pool/snap/home/.gbsnap-probe"}, run.Reply{False: true}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/.gbsnap-probe"}, run.Reply{})
	f.host("").Replies[probeKey("/pool/snap/home/20260807T140000Z")] = run.Reply{Out: dumpUnchanged}

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "snapshot -r /home /pool/snap/home/2026")
	// Whatever the verdict, the throwaway snapshot must be gone.
	f.mustRun("", "subvolume delete /pool/snap/home/.gbsnap-probe")
	// Decoding the dump needs no root, so its runner must not carry sudo.
	if !slices.Contains(f.modes, "|never") {
		t.Errorf("the dump should run without sudo; runners: %v", f.modes)
	}
}

func TestSnapChangedByProbe(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	f.subvolume("/home", shown{uuid: liveUUID, generation: 43})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: liveUUID, generation: 42, readOnly: true})
	f.host("").
		Script([]string{"test", "-e", "/pool/snap/home/.gbsnap-probe"}, run.Reply{False: true}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/" + nowStamp}, run.Reply{})
	f.host("").Replies[probeKey("/pool/snap/home/20260807T140000Z")] = run.Reply{Out: dumpChanged}

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "snapshot -r /home /pool/snap/home/"+nowStamp)
	f.mustRun("", "subvolume delete /pool/snap/home/.gbsnap-probe")
}

// When the probe can't tell whether anything changed, the volume is
// snapshotted regardless, saying why, rather than left out of its backup: the
// probe only decides whether a snapshot is worth taking.
func TestSnapTakenWhenProbeFails(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	f.subvolume("/home", shown{uuid: liveUUID, generation: 43})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: liveUUID, generation: 42, readOnly: true})
	f.host("").
		Script([]string{"test", "-e", "/pool/snap/home/.gbsnap-probe"}, run.Reply{False: true}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/" + nowStamp}, run.Reply{})
	f.host("").Replies[probeKey("/pool/snap/home/20260807T140000Z")] = run.Reply{Err: errors.New("the dump gave up")}

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "snapshot -r /home /pool/snap/home/"+nowStamp)
	f.mustRun("", "subvolume delete /pool/snap/home/.gbsnap-probe")
	if out := f.output(); !strings.Contains(out, "could not tell whether it changed since 20260807T140000Z") ||
		!strings.Contains(out, "the dump gave up") {
		t.Errorf("output = %q", out)
	}
}

// A probe snapshot left behind by an interrupted run is cleared rather than
// colliding with the new one.
func TestSnapClearsLeftoverProbe(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	f.subvolume("/home", shown{uuid: liveUUID, generation: 43})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: liveUUID, generation: 42, readOnly: true})
	f.host("").
		Script([]string{"test", "-e", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/.gbsnap-probe"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/.gbsnap-probe"}, run.Reply{})
	f.host("").Replies[probeKey("/pool/snap/home/20260807T140000Z")] = run.Reply{Out: dumpUnchanged}

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	for _, c := range f.host("").Calls {
		if strings.Contains(c, "subvolume delete /pool/snap/home/.gbsnap-probe") {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("expected the leftover and the new probe to be deleted, got %d deletions", deletes)
	}
}

// A newest snapshot that belongs to some other subvolume gives nothing to
// compare against, so the difference is never probed.
func TestSnapWhenLatestIsOfAnotherSubvolume(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	f.subvolume("/home", shown{uuid: liveUUID, generation: 42})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: "99999999-9999-9999-9999-999999999999", generation: 42, readOnly: true})
	f.host("").Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/" + nowStamp}, run.Reply{})

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", ".gbsnap-probe")
	f.mustRun("", "snapshot -r /home /pool/snap/home/"+nowStamp)
}

func TestSnapForceSkipsChangeDetection(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/" + nowStamp}, run.Reply{})

	if err := f.e.Snap(context.Background(), v, "", true); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "filesystem sync")
	f.mustRun("", "snapshot -r /home /pool/snap/home/"+nowStamp)
}

func TestSnapCreatesPoolAndFirstSnapshot(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.missingPool("/pool/snap/home")
	f.host("").
		Script([]string{"mkdir", "-p", "--", "/pool/snap/home"}, run.Reply{}).
		Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home", "/pool/snap/home/" + nowStamp}, run.Reply{})

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "mkdir -p -- /pool/snap/home")
	f.mustNotRun("", "filesystem sync") // nothing to compare a first snapshot against
	f.mustRun("", "snapshot -r /home /pool/snap/home/"+nowStamp)
}

func TestSnapTagged(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home")
	f.host("").Script([]string{"btrfs", "subvolume", "snapshot", "-r", "/home",
		"/pool/snap/home/" + nowStamp + "-keep"}, run.Reply{})

	if err := f.e.Snap(context.Background(), v, "keep", false); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "/pool/snap/home/"+nowStamp+"-keep")
}

// A clock behind the pool must not name a snapshot into the past, which
// replication would silently never send.
func TestSnapRefusesClockBehindPool(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20270101T000000Z")

	err := f.e.Snap(context.Background(), v, "", true)
	if err == nil || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("Snap = %v", err)
	}
	f.mustNotRun("", "subvolume snapshot")
}

func TestSnapWithoutSubvolumeDoesNothing(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "")
	f.pool("/pool/snap/home", "20260807T140000Z")

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "snapshot")
}

func TestSyncFullSendToEmptyTarget(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.pool("nas:/backup/home")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: snapUUID, readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{
		uuid: "33333333-3333-3333-3333-333333333333", received: snapUUID, readOnly: true})
	f.host("").Replies[run.PipeKey(
		[]string{"btrfs", "send", "/pool/snap/home/20260807T140000Z"},
		run.NewFake("nas"),
		[]string{"btrfs", "receive", "/backup/home"})] = run.Reply{}

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "btrfs send /pool/snap/home/20260807T140000Z | [nas] btrfs receive /backup/home")
	if !strings.Contains(f.output(), "in full") {
		t.Errorf("output = %q", f.output())
	}
}

// Everything the target is missing is sent, each snapshot building on the one
// before it, so that a target that fell behind keeps the whole history.
func TestSyncSendsEveryPendingSnapshot(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.pool("nas:/backup/home", "20260807T140000Z")

	for _, n := range []string{"20260807T140000Z", "20260807T140100Z", "20260807T140200Z"} {
		f.subvolume("/pool/snap/home/"+n, shown{uuid: "src-" + n, readOnly: true})
		f.subvolume("nas:/backup/home/"+n, shown{uuid: "dst-" + n, received: "src-" + n, readOnly: true})
	}
	for _, step := range [][2]string{
		{"20260807T140000Z", "20260807T140100Z"},
		{"20260807T140100Z", "20260807T140200Z"},
	} {
		f.host("").Replies[run.PipeKey(
			[]string{"btrfs", "send", "-p", "/pool/snap/home/" + step[0], "/pool/snap/home/" + step[1]},
			run.NewFake("nas"),
			[]string{"btrfs", "receive", "/backup/home"})] = run.Reply{}
	}

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "send -p /pool/snap/home/20260807T140000Z /pool/snap/home/20260807T140100Z")
	f.mustRun("", "send -p /pool/snap/home/20260807T140100Z /pool/snap/home/20260807T140200Z")
}

func TestSyncUpToDate(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.pool("nas:/backup/home", "20260807T140000Z")

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", "btrfs send")
	f.mustNotRun("nas", "btrfs receive")
}

// A destination snapshot that did not come from this pool must never become the
// parent of an incremental send.
func TestSyncRefusesDivergedParent(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: snapUUID, readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{
		uuid:     "33333333-3333-3333-3333-333333333333",
		received: "44444444-4444-4444-4444-444444444444", readOnly: true})

	err := f.e.Sync(context.Background(), v, SyncOpts{})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("error = %v", err)
	}
	f.mustNotRun("", "btrfs send")
}

// A destination snapshot that was never received at all is equally unusable.
func TestSyncRefusesUnreceivedParent(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")
	f.pool("nas:/backup/home", "20260807T140000Z")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: snapUUID, readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{uuid: snapUUID, readOnly: true})

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err == nil {
		t.Fatal("expected a refusal")
	}
	f.mustNotRun("", "btrfs send")
}

// What arrives is checked before anything downstream trusts it.
func TestSyncRefusesMismatchedArrival(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.pool("nas:/backup/home")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: snapUUID, readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{
		uuid:     "33333333-3333-3333-3333-333333333333",
		received: "55555555-5555-5555-5555-555555555555", readOnly: true})
	f.host("").Replies[run.PipeKey(
		[]string{"btrfs", "send", "/pool/snap/home/20260807T140000Z"},
		run.NewFake("nas"),
		[]string{"btrfs", "receive", "/backup/home"})] = run.Reply{}

	err := f.e.Sync(context.Background(), v, SyncOpts{})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "did not arrive intact") {
		t.Errorf("error = %v", err)
	}
}

// A snapshot passed along a chain of pools carries the UUID it was received
// with, which is what the next hop has to match against.
func TestSyncRelaysReceivedSnapshot(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.pool("nas:/backup/home")
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: "local-uuid", received: "origin-uuid", readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{
		uuid: "remote-uuid", received: "origin-uuid", readOnly: true})
	f.host("").Replies[run.PipeKey(
		[]string{"btrfs", "send", "/pool/snap/home/20260807T140000Z"},
		run.NewFake("nas"),
		[]string{"btrfs", "receive", "/backup/home"})] = run.Reply{}

	if err := f.e.Sync(context.Background(), v, SyncOpts{}); err != nil {
		t.Fatal(err)
	}
}

// One unreachable target does not stop the others being brought up to date.
func TestSyncContinuesPastAFailedTarget(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "broken:/backup/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.missingPool("nas:/backup/home")
	f.host("nas").Script([]string{"mkdir", "-p", "--", "/backup/home"}, run.Reply{})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{uuid: snapUUID, readOnly: true})
	f.subvolume("nas:/backup/home/20260807T140000Z", shown{
		uuid: "33333333-3333-3333-3333-333333333333", received: snapUUID, readOnly: true})
	f.host("").Replies[run.PipeKey(
		[]string{"btrfs", "send", "/pool/snap/home/20260807T140000Z"},
		run.NewFake("nas"),
		[]string{"btrfs", "receive", "/backup/home"})] = run.Reply{}
	// The broken host is left unscripted, so every command on it fails.

	err := f.e.Sync(context.Background(), v, SyncOpts{})
	if err == nil {
		t.Fatal("expected the broken target to be reported")
	}
	if !strings.Contains(err.Error(), "broken:/backup/home") {
		t.Errorf("error should name the failed target, got %v", err)
	}
	f.mustRun("", "| [nas] btrfs receive /backup/home")
}

func TestPrune(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	v.Retention.Pool.Min = 2
	v.Retention.Target.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z", "20260807T140300Z")
	f.pool("nas:/backup/home", "20260807T140000Z", "20260807T140100Z", "20260807T140200Z")
	f.host("").Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/20260807T140000Z"}, run.Reply{})
	f.host("nas").Script([]string{"btrfs", "subvolume", "delete", "/backup/home/20260807T140000Z"}, run.Reply{})
	f.host("nas").Script([]string{"btrfs", "subvolume", "delete", "/backup/home/20260807T140100Z"}, run.Reply{})

	if err := f.e.Prune(context.Background(), v, PruneOpts{}); err != nil {
		t.Fatal(err)
	}
	// 140200Z is the newest snapshot both sides hold, so it is pinned on both:
	// the next incremental send needs it as its parent. Being pinned it does
	// not take up a place in either retention, so the pool still keeps its
	// newest two on top of it, and the target its newest one.
	f.mustRun("", "subvolume delete /pool/snap/home/20260807T140000Z")
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140100Z")
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140200Z")
	f.mustNotRun("", "subvolume delete /pool/snap/home/20260807T140300Z")
	f.mustRun("nas", "subvolume delete /backup/home/20260807T140000Z")
	f.mustNotRun("nas", "subvolume delete /backup/home/20260807T140100Z")
	f.mustNotRun("nas", "subvolume delete /backup/home/20260807T140200Z")
}

func TestPruneKeepsTaggedSnapshots(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z-keep", "20260807T140100Z", "20260807T140200Z")
	f.host("").Script([]string{"btrfs", "subvolume", "delete", "/pool/snap/home/20260807T140100Z"}, run.Reply{})

	if err := f.e.Prune(context.Background(), v, PruneOpts{}); err != nil {
		t.Fatal(err)
	}
	f.mustRun("", "subvolume delete /pool/snap/home/20260807T140100Z")
	f.mustNotRun("", "20260807T140000Z-keep")
}

func TestRunSkipsPruningWhenSyncFails(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "", "broken:/backup/home")
	v.Retention.Pool.Min = 1
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z")

	if err := f.e.Run(context.Background(), v, false); err == nil {
		t.Fatal("expected the failure to be reported")
	}
	// With the target's contents unknown, nothing may be let go of.
	f.mustNotRun("", "subvolume delete")
}

func TestDryRunChangesNothing(t *testing.T) {
	f := newFixture(t, true)
	v := volume(t, "/home", "nas:/backup/home")
	v.Retention.Pool.Min = 1
	f.missingPool("/pool/snap/home")
	f.missingPool("nas:/backup/home")

	if err := f.e.Run(context.Background(), v, false); err != nil {
		t.Fatal(err)
	}
	for host, fake := range f.hosts {
		for _, c := range fake.Calls {
			for _, forbidden := range []string{"mkdir", "subvolume snapshot", "subvolume delete", "btrfs send", "btrfs receive", "tee", "mv"} {
				if strings.Contains(c, forbidden) {
					t.Errorf("dry run touched host %q: %s", host, c)
				}
			}
		}
	}
	out := f.output()
	for _, want := range []string{
		"would create pool /pool/snap/home",
		"would snapshot /home to /pool/snap/home/" + nowStamp,
		"would create pool nas:/backup/home",
		"would send /pool/snap/home/" + nowStamp + " to nas:/backup/home in full",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run should report %q, got:\n%s", want, out)
		}
	}
}

// A dry run still reads and reports, including the fact that it stopped short
// of the one check that would have had to write.
func TestDryRunDoesNotProbe(t *testing.T) {
	f := newFixture(t, true)
	v := volume(t, "/home")
	f.pool("/pool/snap/home", "20260807T140000Z")
	f.host("").Script([]string{"btrfs", "filesystem", "sync", "/home"}, run.Reply{})
	f.subvolume("/home", shown{uuid: liveUUID, generation: 43})
	f.subvolume("/pool/snap/home/20260807T140000Z", shown{
		uuid: snapUUID, parent: liveUUID, generation: 42, readOnly: true})

	if err := f.e.Snap(context.Background(), v, "", false); err != nil {
		t.Fatal(err)
	}
	f.mustNotRun("", ".gbsnap-probe")
	if !strings.Contains(f.output(), "would examine /home for changes") {
		t.Errorf("output = %q", f.output())
	}
}

// A pool named on the command line has to exist: a mistyped path must not read
// as an empty pool and succeed.
func TestAdHocRefusesMissingPool(t *testing.T) {
	f := newFixture(t, false)
	v := config.AdHoc(parse(t, "/pool/absent"), nil, 1, run.SudoNever)
	f.missingPool("/pool/absent")

	err := f.e.List(context.Background(), v)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("List = %v", err)
	}
}

func TestListAndStatus(t *testing.T) {
	f := newFixture(t, false)
	v := volume(t, "/home", "nas:/backup/home")
	f.pool("/pool/snap/home", "20260807T140000Z", "20260807T140100Z-keep", "20260807T140200Z")
	f.pool("nas:/backup/home", "20260807T140000Z")

	if err := f.e.List(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	out := f.output()
	if !strings.Contains(out, "20260807T140100Z-keep") || !strings.Contains(out, "keep") {
		t.Errorf("list should mark the protected snapshot:\n%s", out)
	}
	if !strings.Contains(out, "nas:/backup/home") {
		t.Errorf("list should say which target holds a snapshot:\n%s", out)
	}

	f.out.Reset()
	if err := f.e.Status(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	out = f.output()
	if !strings.Contains(out, "3 snapshots") || !strings.Contains(out, "1 snapshot") {
		t.Errorf("status should count both sides:\n%s", out)
	}
	if !strings.Contains(out, "2 snapshots to send") {
		t.Errorf("status should report the backlog:\n%s", out)
	}
}
