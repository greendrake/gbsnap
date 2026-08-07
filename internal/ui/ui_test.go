package ui

import (
	"bytes"
	"testing"
)

func TestLevels(t *testing.T) {
	for name, tc := range map[string]struct {
		verbose, quiet  bool
		action, detail  bool
		wantErrorAnyway bool
	}{
		"by default actions are reported and details are not": {false, false, true, false, true},
		"verbose adds the details":                            {true, false, true, true, true},
		"quiet leaves only errors":                            {false, true, false, false, true},
		"quiet outranks verbose":                              {true, true, false, false, true},
	} {
		var out, errw bytes.Buffer
		p := &Printer{Out: &out, Err: &errw, Verbose: tc.verbose, Quiet: tc.quiet}
		p.Action("did %s", "something")
		p.Detail("ran %s", "a command")
		p.Error("it went %s", "wrong")

		if got := bytes.Contains(out.Bytes(), []byte("did something")); got != tc.action {
			t.Errorf("%s: action reported = %v", name, got)
		}
		if got := bytes.Contains(out.Bytes(), []byte("ran a command")); got != tc.detail {
			t.Errorf("%s: detail reported = %v", name, got)
		}
		if got := bytes.Contains(errw.Bytes(), []byte("gbsnap: it went wrong")); got != tc.wantErrorAnyway {
			t.Errorf("%s: error reported = %v", name, got)
		}
	}
}
