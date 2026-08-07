package pool

// Goes with legacy.go.

import "testing"

func TestParseLegacyName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-08-02.0001", "20260802T000000Z"},
		{"2026-07-11.0001-keep", "20260711T000000Z-keep"},
		{"2026-08-02.0002", "20260802T000000Z.2"},
		{"2026-08-02.0017", "20260802T000000Z.17"},
		{"2026-08-02.0003-pre.upgrade", "20260802T000000Z.3-pre.upgrade"},
		{"2026-01-01.1", "20260101T000000Z"},
	} {
		n, err := ParseLegacyName(tc.in)
		if err != nil {
			t.Errorf("ParseLegacyName(%q): %v", tc.in, err)
			continue
		}
		if got := n.String(); got != tc.want {
			t.Errorf("ParseLegacyName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// What comes out has to be a name this tool can read back.
		if _, err := ParseName(n.String()); err != nil {
			t.Errorf("ParseLegacyName(%q) produced %q, which is not a snapshot name: %v", tc.in, n, err)
		}
	}
}

func TestParseLegacyNameRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"20260807T143205Z",   // already this tool's
		"2026-08-02",         // no sequence number
		"2026-08-02.0000",    // the predecessor numbered from one
		"2026-13-02.0001",    // month 13
		"2026-08-32.0001",    // day 32
		"2026-08-02.0001-",   // empty tag
		"2026-08-02.0001-.x", // tag must start with a letter or digit
		"snapshot",
		"2026-08-02.0001 extra",
		"prefix2026-08-02.0001",
	} {
		if n, err := ParseLegacyName(in); err == nil {
			t.Errorf("ParseLegacyName(%q) accepted as %+v", in, n)
		}
	}
}

// Converting must not disturb the order the predecessor kept its snapshots in,
// or replication would send them out of sequence.
func TestParseLegacyNameKeepsOrder(t *testing.T) {
	legacy := []string{
		"2026-07-11.0001", "2026-07-11.0002", "2026-07-11.0010",
		"2026-07-30.0001", "2026-08-01.0001", "2026-08-02.0002",
	}
	var previous Name
	for i, s := range legacy {
		n, err := ParseLegacyName(s)
		if err != nil {
			t.Fatalf("ParseLegacyName(%q): %v", s, err)
		}
		if i > 0 && !previous.Before(n) {
			t.Errorf("%s converted to %s, which does not follow %s", s, n, previous)
		}
		previous = n
	}
}

// Two predecessor names must never converge on one converted name, or a
// snapshot would be lost to the one that took its place.
func TestParseLegacyNameIsOneToOne(t *testing.T) {
	seen := map[string]string{}
	for _, day := range []string{"2026-07-11", "2026-08-02"} {
		for _, seq := range []string{"0001", "0002", "0003", "0010", "1"} {
			in := day + "." + seq
			n, err := ParseLegacyName(in)
			if err != nil {
				t.Fatalf("ParseLegacyName(%q): %v", in, err)
			}
			// "0001" and "1" are the same sequence number written two ways,
			// so they are meant to agree.
			if first, ok := seen[n.String()]; ok && first != day+".0001" && seq != "1" {
				t.Errorf("%q and %q both convert to %s", first, in, n)
			}
			seen[n.String()] = in
		}
	}
}
