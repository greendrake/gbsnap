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

func TestReadPlace(t *testing.T) {
	ctx := context.Background()
	f := run.NewFake("nas").
		Script([]string{"test", "-f", "/backup/.gbsnap-place"}, run.Reply{}).
		Script([]string{"cat", "--", "/backup/.gbsnap-place"}, run.Reply{Out: "0123456789abcdef0123456789abcdef\n"}).
		Script([]string{"test", "-f", "/bare/.gbsnap-place"}, run.Reply{False: true}).
		Script([]string{"test", "-f", "/odd/.gbsnap-place"}, run.Reply{}).
		Script([]string{"cat", "--", "/odd/.gbsnap-place"}, run.Reply{Out: "hello\n"})

	if id, err := ReadPlace(ctx, f, "/backup"); err != nil || id != "0123456789abcdef0123456789abcdef" {
		t.Errorf("ReadPlace(/backup) = %q, %v", id, err)
	}
	if id, err := ReadPlace(ctx, f, "/bare"); err != nil || id != "" {
		t.Errorf("ReadPlace(/bare) = %q, %v", id, err)
	}
	if _, err := ReadPlace(ctx, f, "/odd"); err == nil {
		t.Error("a marker holding no ID should be refused")
	}

	a, err := NewPlaceID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewPlaceID()
	if !placeIDRe.MatchString(a) || a == b {
		t.Errorf("IDs %q and %q", a, b)
	}
}
