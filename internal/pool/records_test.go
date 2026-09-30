package pool

import (
	"context"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/run"
)

func TestRecordsSet(t *testing.T) {
	rs := &Records{}
	then := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	later := then.Add(time.Hour)
	if !rs.Set(Record{Target: "a", Location: "/d/a", Snapshot: "20260807T140000Z", Since: then}) {
		t.Error("a new record is a change")
	}
	if rs.Set(Record{Target: "a", Location: "/d/a", Snapshot: "20260807T140000Z", Since: later}) {
		t.Error("the same snapshot at the same place is no change")
	}
	// Reached at another address, the target still holds what it held since
	// it received it.
	if !rs.Set(Record{Target: "a", Location: "nas:/d/a", Snapshot: "20260807T140000Z", Since: later}) {
		t.Error("a new address is a change")
	}
	if r, _ := rs.Get("a"); !r.Since.Equal(then) || r.Location != "nas:/d/a" {
		t.Errorf("record = %+v", r)
	}
	rs.Set(Record{Target: "a", Location: "nas:/d/a", Snapshot: "20260807T140100Z", Since: later})
	if r, _ := rs.Get("a"); !r.Since.Equal(later) || len(rs.List) != 1 {
		t.Errorf("records = %+v", rs.List)
	}
}

func TestRecordsDropAndRename(t *testing.T) {
	rs := &Records{List: []Record{
		{Target: "a", Snapshot: "20260807T140000Z-keep"},
		{Target: "b", Snapshot: "20260807T140000Z-keep"},
		{Target: "c", Snapshot: "20260807T140100Z"},
	}}
	from, _ := ParseName("20260807T140000Z-keep")
	if !rs.Rename(from, from.Untagged()) {
		t.Error("Rename should report the change")
	}
	if rs.List[0].Snapshot != "20260807T140000Z" || rs.List[1].Snapshot != "20260807T140000Z" {
		t.Errorf("records = %+v", rs.List)
	}
	dropped := rs.Drop(func(r Record) bool { return r.Target != "c" })
	if len(dropped) != 2 || len(rs.List) != 1 || rs.List[0].Target != "c" {
		t.Errorf("dropped %+v, kept %+v", dropped, rs.List)
	}
}

func TestLoadAndSaveRecords(t *testing.T) {
	ctx := context.Background()
	f := run.NewFake("")
	p := New(loc(t, "/pool/snap"), f)

	// A pool not there yet knows nothing, and reads nothing to find that out.
	f.Script([]string{"test", "-d", "/pool/snap"}, run.Reply{False: true})
	if err := p.Load(ctx); err != nil {
		t.Fatal(err)
	}
	rs, err := p.LoadRecords(ctx)
	if err != nil || len(rs.List) != 0 {
		t.Fatalf("LoadRecords = %+v, %v", rs, err)
	}

	f.Script([]string{"test", "-d", "/pool/snap"}, run.Reply{}).
		Script([]string{"ls", "-1", "--", "/pool/snap"}, run.Reply{Out: ""}).
		Script([]string{"test", "-f", "/pool/snap/.gbsnap-targets"}, run.Reply{}).
		Script([]string{"cat", "--", "/pool/snap/.gbsnap-targets"},
			run.Reply{Out: `{"targets":[{"target":"a","location":"/d/a","snapshot":"20260807T140000Z"}]}`}).
		Script([]string{"tee", "--", "/pool/snap/.gbsnap-targets.new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", "/pool/snap/.gbsnap-targets.new", "/pool/snap/.gbsnap-targets"}, run.Reply{})
	if err := p.Load(ctx); err != nil {
		t.Fatal(err)
	}
	rs, err = p.LoadRecords(ctx)
	if err != nil || len(rs.List) != 1 || rs.List[0].Snapshot != "20260807T140000Z" {
		t.Fatalf("LoadRecords = %+v, %v", rs, err)
	}
	if err := p.SaveRecords(ctx, rs); err != nil {
		t.Fatal(err)
	}
	if written := f.Inputs["tee -- /pool/snap/.gbsnap-targets.new"]; written == "" {
		t.Error("nothing was written")
	}
}

func TestID(t *testing.T) {
	ctx := context.Background()
	f := run.NewFake("nas").
		Script([]string{"test", "-d", "/backup/home"}, run.Reply{}).
		Script([]string{"ls", "-1", "--", "/backup/home"}, run.Reply{}).
		Script([]string{"test", "-f", "/backup/home/.gbsnap-id"}, run.Reply{}).
		Script([]string{"cat", "--", "/backup/home/.gbsnap-id"}, run.Reply{Out: "0123456789abcdef0123456789abcdef\n"}).
		Script([]string{"test", "-d", "/odd"}, run.Reply{}).
		Script([]string{"ls", "-1", "--", "/odd"}, run.Reply{}).
		Script([]string{"test", "-f", "/odd/.gbsnap-id"}, run.Reply{}).
		Script([]string{"cat", "--", "/odd/.gbsnap-id"}, run.Reply{Out: "hello\n"}).
		Script([]string{"test", "-d", "/new"}, run.Reply{}).
		Script([]string{"ls", "-1", "--", "/new"}, run.Reply{}).
		Script([]string{"test", "-f", "/new/.gbsnap-id"}, run.Reply{False: true}).
		Script([]string{"tee", "--", "/new/.gbsnap-id.new"}, run.Reply{}).
		Script([]string{"mv", "-T", "--", "/new/.gbsnap-id.new", "/new/.gbsnap-id"}, run.Reply{}).
		Script([]string{"test", "-d", "/absent"}, run.Reply{False: true})
	load := func(path string) *Pool {
		p := New(loc(t, "nas:"+path), f)
		if err := p.Load(ctx); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if id, err := load("/backup/home").ID(ctx); err != nil || id != "0123456789abcdef0123456789abcdef" {
		t.Errorf("ID = %q, %v", id, err)
	}
	if _, err := load("/odd").ID(ctx); err == nil {
		t.Error("an ID file holding no ID should be refused")
	}
	p := load("/new")
	if id, err := p.ID(ctx); err != nil || id != "" {
		t.Errorf("ID of a pool without one = %q, %v", id, err)
	}
	if err := p.SetID(ctx, "fedcba9876543210fedcba9876543210"); err != nil {
		t.Fatal(err)
	}
	if got := f.Inputs["tee -- /new/.gbsnap-id.new"]; got != "fedcba9876543210fedcba9876543210\n" {
		t.Errorf("wrote %q", got)
	}
	// A pool that is not there has no ID, and nothing is read to find that out.
	if id, err := load("/absent").ID(ctx); err != nil || id != "" {
		t.Errorf("ID of a missing pool = %q, %v", id, err)
	}

	a, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewID()
	if !IsID(a) || a == b {
		t.Errorf("IDs %q and %q", a, b)
	}
}
