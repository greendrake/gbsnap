package pool

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestTimeLayoutIsLiteralZ(t *testing.T) {
	// The trailing Z must be part of the text, not a zone designator, or names
	// would render differently depending on the machine's clock setting.
	parsed := at("20260807T143205Z")
	if parsed.Location() != time.UTC {
		t.Errorf("parsed location = %v", parsed.Location())
	}
	if got := parsed.Format(TimeLayout); got != "20260807T143205Z" {
		t.Errorf("round trip = %q", got)
	}
	if y, m, d := parsed.Date(); y != 2026 || m != time.August || d != 7 {
		t.Errorf("date = %d-%d-%d", y, m, d)
	}
	if h, min, s := parsed.Clock(); h != 14 || min != 32 || s != 5 {
		t.Errorf("clock = %d:%d:%d", h, min, s)
	}
}

func TestParseName(t *testing.T) {
	for _, tc := range []struct {
		in      string
		ordinal int
		tag     string
	}{
		{"20260807T143205Z", 0, ""},
		{"20260807T143205Z.2", 2, ""},
		{"20260807T143205Z.17", 17, ""},
		{"20260807T143205Z-keep", 0, "keep"},
		{"20260807T143205Z-pre.upgrade", 0, "pre.upgrade"},
		{"20260807T143205Z.3-keep", 3, "keep"},
	} {
		n, err := ParseName(tc.in)
		if err != nil {
			t.Errorf("ParseName(%q): %v", tc.in, err)
			continue
		}
		if n.Ordinal != tc.ordinal || n.Tag != tc.tag {
			t.Errorf("ParseName(%q) = %+v, want ordinal %d tag %q", tc.in, n, tc.ordinal, tc.tag)
		}
		if got := n.String(); got != tc.in {
			t.Errorf("round trip of %q gave %q", tc.in, got)
		}
		if n.Protected() != (tc.tag != "") {
			t.Errorf("%q: Protected = %v", tc.in, n.Protected())
		}
	}
}

func TestParseNameRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"snapshot",
		"2026-08-07.0001",              // the shape another tool would use
		"20260807T143205",              // no zone marker
		"20260807T143205Z.1",           // an ordinal of 1 is never written
		"20260807T143205Z.0",           // nor is 0
		"20260807T143205Z.02",          // nor a padded one
		"20260807T143205Z-",            // empty tag
		"20260807T143205Z-.hidden",     // tag must start with a letter or digit
		"20261307T143205Z",             // month 13
		"20260832T143205Z",             // day 32
		"20260807T253205Z",             // hour 25
		"20260807T143205Z extra",       //
		"prefix20260807T143205Z",       //
		"20260807T143205Z-keep/nested", //
	} {
		if n, err := ParseName(in); err == nil {
			t.Errorf("ParseName(%q) accepted as %+v", in, n)
		}
	}
}

func TestValidateTag(t *testing.T) {
	for _, ok := range []string{"keep", "pre-upgrade", "v1.2", "a"} {
		if err := ValidateTag(ok); err != nil {
			t.Errorf("ValidateTag(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-keep", ".keep", "with space", "sla/sh", "über"} {
		if err := ValidateTag(bad); err == nil {
			t.Errorf("ValidateTag(%q) accepted", bad)
		}
	}
}

func TestBefore(t *testing.T) {
	older := Name{Time: at("20260807T143205Z")}
	newer := Name{Time: at("20260807T143206Z")}
	if !older.Before(newer) || newer.Before(older) {
		t.Error("timestamps should order")
	}
	first := Name{Time: at("20260807T143205Z")}
	second := Name{Time: at("20260807T143205Z"), Ordinal: 2}
	tenth := Name{Time: at("20260807T143205Z"), Ordinal: 10}
	if !first.Before(second) || !second.Before(tenth) {
		t.Error("ordinals should order numerically, not as text")
	}
	if first.Before(first) {
		t.Error("a name should not precede itself")
	}
	// Names gbsnap creates never tie on timestamp and ordinal, but the order still
	// has to be total for sorting to be deterministic.
	untagged := Name{Time: at("20260807T143205Z")}
	kept := Name{Time: at("20260807T143205Z"), Tag: "keep"}
	if !untagged.Before(kept) || kept.Before(untagged) {
		t.Error("a tag should settle a tie")
	}
}

func TestNextName(t *testing.T) {
	now := at("20260807T143205Z")

	if got := NextName(now, nil, "").String(); got != "20260807T143205Z" {
		t.Errorf("empty pool gave %q", got)
	}
	if got := NextName(now, nil, "keep").String(); got != "20260807T143205Z-keep" {
		t.Errorf("tagged gave %q", got)
	}

	existing := []Name{{Time: at("20260807T143205Z")}}
	if got := NextName(now, existing, "").String(); got != "20260807T143205Z.2" {
		t.Errorf("first collision gave %q", got)
	}

	// A tag does not make a name distinct enough: the ordinal keeps the
	// timestamp and ordinal pair unique so that ordering never depends on tags.
	tagged := []Name{{Time: at("20260807T143205Z"), Tag: "keep"}}
	if got := NextName(now, tagged, "").String(); got != "20260807T143205Z.2" {
		t.Errorf("collision with a tagged name gave %q", got)
	}

	crowded := []Name{
		{Time: at("20260807T143205Z")},
		{Time: at("20260807T143205Z"), Ordinal: 2},
		{Time: at("20260807T143205Z"), Ordinal: 3},
		{Time: at("20260807T143204Z")},
	}
	if got := NextName(now, crowded, "").String(); got != "20260807T143205Z.4" {
		t.Errorf("crowded second gave %q", got)
	}

	// A gap left by pruning is not filled: a name is required to sort after
	// everything the pool holds, not merely to be free, or replication would
	// never reach it.
	gapped := []Name{
		{Time: at("20260807T143205Z")},
		{Time: at("20260807T143205Z"), Ordinal: 3},
	}
	if got := NextName(now, gapped, "").String(); got != "20260807T143205Z.4" {
		t.Errorf("gapped second gave %q", got)
	}

	// The name of a snapshot taken within the same second as the pool's latest
	// always sorts after it, whatever the pool holds.
	for _, existing := range [][]Name{
		{{Time: at("20260807T143205Z")}},
		{{Time: at("20260807T143205Z"), Ordinal: 3}},
		{{Time: at("20260807T143205Z"), Tag: "keep"}, {Time: at("20260807T143205Z"), Ordinal: 3}},
		{{Time: at("20260807T143204Z")}, {Time: at("20260807T143205Z"), Ordinal: 9}},
	} {
		next := NextName(now, existing, "")
		for _, e := range existing {
			if !e.Before(next) {
				t.Errorf("NextName over %v gave %s, which does not sort after %s",
					existing, next, e)
			}
		}
	}

	// Sub-second precision is dropped, and a local clock renders as UTC.
	precise := time.Date(2026, 8, 7, 14, 32, 5, 999999999, time.FixedZone("NZST", 12*3600))
	if got := NextName(precise, nil, "").String(); got != "20260807T023205Z" {
		t.Errorf("local time gave %q", got)
	}
}
