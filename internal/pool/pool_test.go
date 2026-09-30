package pool

import (
	"context"
	"strings"
	"testing"

	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/run"
)

func loc(t *testing.T, s string) location.Location {
	t.Helper()
	l, err := location.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func names(ns []Name) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.String()
	}
	return out
}

func TestLoad(t *testing.T) {
	f := run.NewFake("nas")
	f.Script([]string{"test", "-d", "/backup/snap"}, run.Reply{})
	// Deliberately out of order: the pool sorts what the listing gives it.
	f.Script([]string{"ls", "-1", "--", "/backup/snap"}, run.Reply{
		Out: "20260807T143206Z\n20260807T143205Z-keep\n20260807T143205Z\n",
	})

	p := New(loc(t, "nas:/backup/snap"), f)
	if err := p.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.Exists() {
		t.Error("pool should exist")
	}
	got := strings.Join(names(p.Names()), " ")
	want := "20260807T143205Z 20260807T143205Z-keep 20260807T143206Z"
	if got != want {
		t.Errorf("Names = %q, want %q", got, want)
	}
	latest, ok := p.Latest()
	if !ok || latest.String() != "20260807T143206Z" {
		t.Errorf("Latest = %v %v", latest, ok)
	}
	if got := p.Path(latest); got != "/backup/snap/20260807T143206Z" {
		t.Errorf("Path = %q", got)
	}
	if got := p.ProbePath(); got != "/backup/snap/.gbsnap-probe" {
		t.Errorf("ProbePath = %q", got)
	}
}

func TestLoadMissingPool(t *testing.T) {
	f := run.NewFake("")
	f.Script([]string{"test", "-d", "/pool/snap"}, run.Reply{False: true})

	p := New(loc(t, "/pool/snap"), f)
	if err := p.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Exists() {
		t.Error("pool should not exist")
	}
	if len(p.Names()) != 0 {
		t.Errorf("Names = %v", p.Names())
	}
	if _, ok := p.Latest(); ok {
		t.Error("empty pool has no latest")
	}
}

// An entry gbsnap does not recognise is refused rather than ignored: it may be
// a snapshot from another tool that pruning must never be trusted to reason
// about.
func TestLoadRejectsStrayEntries(t *testing.T) {
	f := run.NewFake("")
	f.Script([]string{"test", "-d", "/pool/snap"}, run.Reply{})
	f.Script([]string{"ls", "-1", "--", "/pool/snap"}, run.Reply{
		Out: "20260807T143205Z\n2026-08-07.0001\nnotes.txt\n",
	})

	err := New(loc(t, "/pool/snap"), f).Load(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"2026-08-07.0001", "notes.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got %v", want, err)
		}
	}
}

func TestAddRemove(t *testing.T) {
	p := New(loc(t, "/pool/snap"), run.NewFake(""))
	older := Name{Time: at("20260807T143205Z")}
	newer := Name{Time: at("20260807T143206Z")}

	p.Add(newer)
	p.Add(older)
	if got := strings.Join(names(p.Names()), " "); got != "20260807T143205Z 20260807T143206Z" {
		t.Errorf("Add kept the wrong order: %q", got)
	}
	if !p.Exists() {
		t.Error("adding a snapshot implies the pool exists")
	}

	p.Remove(older)
	if got := strings.Join(names(p.Names()), " "); got != "20260807T143206Z" {
		t.Errorf("after Remove: %q", got)
	}
}

func TestCommon(t *testing.T) {
	src := []Name{
		{Time: at("20260807T143201Z")},
		{Time: at("20260807T143202Z")},
		{Time: at("20260807T143203Z")},
		{Time: at("20260807T143204Z")},
	}
	dst := []Name{
		{Time: at("20260807T143201Z")},
		{Time: at("20260807T143203Z")},
		{Time: at("20260807T143209Z")}, // only at the destination
	}
	got, ok := Common(src, dst)
	if !ok || got.String() != "20260807T143203Z" {
		t.Errorf("Common = %v %v, want the newest shared name", got, ok)
	}

	if _, ok := Common(src, nil); ok {
		t.Error("nothing in common should report false")
	}

	// A tag is a label, not part of what the snapshot is: a copy is shared
	// whatever tag either side carries, and named as the source names it.
	tagged := []Name{{Time: at("20260807T143201Z"), Tag: "keep"}}
	if got, ok := Common(src, tagged); !ok || got.String() != "20260807T143201Z" {
		t.Errorf("Common across a tag = %v %v", got, ok)
	}
	// The ordinal is part of it, though.
	if _, ok := Common(src, []Name{{Time: at("20260807T143201Z"), Ordinal: 2}}); ok {
		t.Error("another ordinal is another snapshot")
	}
}

func TestAfter(t *testing.T) {
	all := []Name{
		{Time: at("20260807T143201Z")},
		{Time: at("20260807T143202Z")},
		{Time: at("20260807T143203Z")},
	}
	got := After(all, Name{Time: at("20260807T143201Z")}, true)
	if strings.Join(names(got), " ") != "20260807T143202Z 20260807T143203Z" {
		t.Errorf("After = %v", names(got))
	}
	if got := After(all, Name{}, false); len(got) != 3 {
		t.Errorf("without a parent everything is pending, got %v", names(got))
	}
	if got := After(all, all[2], true); len(got) != 0 {
		t.Errorf("nothing follows the newest, got %v", names(got))
	}
	// A snapshot does not follow itself under another tag.
	if got := After(all, Name{Time: at("20260807T143203Z"), Tag: "keep"}, true); len(got) != 0 {
		t.Errorf("nothing follows the newest, tagged, got %v", names(got))
	}
}

func TestHolds(t *testing.T) {
	p := &Pool{names: []Name{{Time: at("20260807T143201Z"), Tag: "keep"}, {Time: at("20260807T143202Z")}}}
	if got, ok := p.Holds(Name{Time: at("20260807T143201Z")}); !ok || got.Tag != "keep" {
		t.Errorf("Holds = %v %v, want the pool's own, tagged name", got, ok)
	}
	if _, ok := p.Holds(Name{Time: at("20260807T143203Z")}); ok {
		t.Error("Holds of a snapshot the pool lacks")
	}
}
