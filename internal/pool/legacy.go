package pool

// The naming this tool's predecessor wrote, which "gbsnap convert" translates.
// This file goes when no pool is left carrying it.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LegacyLayout is the date a predecessor snapshot was named after, once its
// separators have been settled.
const LegacyLayout = "2006-01-02"

// legacyRe matches a predecessor name: the date, the sequence number within
// that date, and the tag that protected it from retention. The predecessor
// spelled the date two ways over its life, 2026-08-02 and 2023.06.06, and
// numbered the sequence to whatever width it felt like, so both are read. The
// two spellings are alternatives rather than a character class, so that a name
// mixing them is not quietly accepted as though it meant something.
var legacyRe = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}|\d{4}\.\d{2}\.\d{2})\.(\d+)(?:-([A-Za-z0-9][A-Za-z0-9._-]*))?$`)

// ParseLegacyName reads a name the predecessor wrote and returns the name this
// tool holds the same snapshot under.
//
// The predecessor recorded the date and nothing finer, so there is no time of
// day in the name to recover: a converted snapshot lands on midnight UTC of
// its date, and the sequence number within the date becomes the ordinal that
// orders it there. The synthetic time costs nothing, since btrfs records the
// true creation time regardless, and deriving the name from the old name alone
// — never from anything the filesystem holds — is what makes conversion safe:
// a pool and each of its targets then arrive at identical names, and the
// snapshots they share go on being recognised as shared.
//
// Both of the predecessor's spellings land on the same converted name, since
// only the date they carry means anything: 2023-06-06.01 and 2023.06.06.01
// both become 20230606T000000Z. A pool holding both spellings of one date and
// sequence therefore cannot be converted, which convert reports as the
// collision it is rather than letting one snapshot displace the other.
func ParseLegacyName(s string) (Name, error) {
	m := legacyRe.FindStringSubmatch(s)
	if m == nil {
		return Name{}, fmt.Errorf(
			"%q is not a name this tool's predecessor wrote (want %s.NNNN[-tag] or 2006.01.02.NN[-tag])",
			s, LegacyLayout)
	}
	// Only the date matters, so the two spellings are settled into one before
	// it is read.
	day, err := time.Parse(LegacyLayout, strings.ReplaceAll(m[1], ".", "-"))
	if err != nil {
		return Name{}, fmt.Errorf("%q: %w", s, err)
	}
	seq, err := strconv.Atoi(m[2])
	if err != nil {
		return Name{}, fmt.Errorf("%q: %w", s, err)
	}
	if seq < 1 {
		return Name{}, fmt.Errorf("%q: sequence number below 1", s)
	}
	// The predecessor numbered from one. Here the first snapshot of a second
	// wears no ordinal at all and the next is 2, so one maps to none and every
	// later number keeps its value, which keeps the mapping one to one and the
	// order intact. The width it was written to carries no meaning: .01 and
	// .0001 are both the first of their day.
	n := Name{Time: day, Tag: m[3]}
	if seq > 1 {
		n.Ordinal = seq
	}
	return n, nil
}
