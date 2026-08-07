package location

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		host string
		path string
	}{
		{"/pool/snap/home", "", "/pool/snap/home"},
		{"snap/home", "", "snap/home"},
		{"nas:/backup/snap", "nas", "/backup/snap"},
		{"user@nas:/backup", "user@nas", "/backup"},
		{"nas:backup", "nas", "backup"},
		{"/pool/odd:name", "", "/pool/odd:name"},
		{"./rel:name", "", "./rel:name"},
	} {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if got.Host != tc.host || got.Path != tc.path {
			t.Errorf("Parse(%q) = %+v, want host %q path %q", tc.in, got, tc.host, tc.path)
		}
		if round := got.String(); round != tc.in {
			t.Errorf("Parse(%q).String() = %q", tc.in, round)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", ":path", "host:"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
}

func TestChildAndEqual(t *testing.T) {
	l, err := Parse("nas:/backup/snap")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Child("20260807T143205Z").String(); got != "nas:/backup/snap/20260807T143205Z" {
		t.Errorf("Child = %q", got)
	}
	trailing, err := Parse("nas:/backup/snap/")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Equal(trailing) {
		t.Error("trailing slash should compare equal")
	}
	if got := trailing.Canonical(); got != "nas:/backup/snap" {
		t.Errorf("Canonical = %q", got)
	}
	if l.Equal(Location{Path: "/backup/snap"}) {
		t.Error("remote should not equal local")
	}
	if !l.Remote() {
		t.Error("expected remote")
	}
}
