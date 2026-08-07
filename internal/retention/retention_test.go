package retention

import (
	"strings"
	"testing"
	"time"

	"github.com/greendrake/gbsnap/internal/pool"
)

// seq builds a run of snapshots one second apart, oldest first. A name ending
// in "!" is tagged, and so protected.
func seq(specs ...string) []pool.Name {
	var out []pool.Name
	base := time.Date(2026, 8, 7, 14, 32, 0, 0, time.UTC)
	for i, s := range specs {
		n := pool.Name{Time: base.Add(time.Duration(i) * time.Second)}
		if strings.HasSuffix(s, "!") {
			n.Tag = "keep"
		}
		out = append(out, n)
	}
	return out
}

func render(ns []pool.Name) string {
	var parts []string
	for _, n := range ns {
		parts = append(parts, n.String())
	}
	return strings.Join(parts, " ")
}

func TestSelect(t *testing.T) {
	plain := seq("a", "b", "c", "d", "e")

	for name, tc := range map[string]struct {
		names  []pool.Name
		min    int
		pinned []string
		want   string
	}{
		"keeps the newest three": {
			plain, 3, nil,
			"20260807T143200Z 20260807T143201Z",
		},
		"nothing to do when the pool is small enough": {
			seq("a", "b", "c"), 3, nil, "",
		},
		"exactly at the limit": {
			seq("a", "b", "c", "d"), 3, nil, "20260807T143200Z",
		},
		"an empty pool": {
			nil, 3, nil, "",
		},
		"tagged snapshots are untouchable and do not fill the quota": {
			// The two tagged snapshots survive, and the three newest prunable
			// ones survive alongside them.
			seq("a!", "b", "c!", "d", "e", "f"), 3, nil,
			"20260807T143201Z",
		},
		"a pool of nothing but tagged snapshots": {
			seq("a!", "b!", "c!", "d!"), 1, nil, "",
		},
		"a pinned parent survives however old it is": {
			plain, 2, []string{"20260807T143200Z"},
			"20260807T143201Z 20260807T143202Z",
		},
		"pinning does not cost a recent snapshot its place": {
			plain, 3, []string{"20260807T143200Z"},
			"20260807T143201Z",
		},
		"several targets pin several parents": {
			plain, 1, []string{"20260807T143200Z", "20260807T143202Z"},
			"20260807T143201Z 20260807T143203Z",
		},
		"a policy of one": {
			plain, 1, nil,
			"20260807T143200Z 20260807T143201Z 20260807T143202Z 20260807T143203Z",
		},
	} {
		pinned := map[string]bool{}
		for _, p := range tc.pinned {
			pinned[p] = true
		}
		got := render(Select(tc.names, Policy{Min: tc.min}, pinned))
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}
