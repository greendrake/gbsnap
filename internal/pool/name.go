package pool

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// TimeLayout is the UTC timestamp a snapshot is named after. It sorts
// lexically, carries no colon so it is safe in a host:path and in a shell, and
// is precise to the second.
const TimeLayout = "20060102T150405Z"

// nameRe matches a timestamp, the ordinal that separates snapshots taken
// within the same second, and the tag that protects one from pruning.
var nameRe = regexp.MustCompile(`^(\d{8}T\d{6}Z)(?:\.(\d+))?(?:-([A-Za-z0-9][A-Za-z0-9._-]*))?$`)

// tagRe matches an acceptable tag on its own.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Name is the name of one snapshot in a pool.
type Name struct {
	Time time.Time
	// Ordinal is 0 when the timestamp alone identifies the snapshot, and 2 or
	// more when an earlier snapshot claimed the same second.
	Ordinal int
	// Tag protects the snapshot from pruning; empty means prunable.
	Tag string
}

// ParseName reads a snapshot name.
func ParseName(s string) (Name, error) {
	m := nameRe.FindStringSubmatch(s)
	if m == nil {
		return Name{}, fmt.Errorf("%q is not a snapshot name (want %s[.ordinal][-tag])", s, TimeLayout)
	}
	t, err := time.Parse(TimeLayout, m[1])
	if err != nil {
		return Name{}, fmt.Errorf("%q: %w", s, err)
	}
	n := Name{Time: t, Tag: m[3]}
	if m[2] != "" {
		// An ordinal of 0 or 1 is never written, so seeing one means the name
		// was not produced by gbsnap.
		if m[2][0] == '0' {
			return Name{}, fmt.Errorf("%q: ordinal has a leading zero", s)
		}
		n.Ordinal, err = strconv.Atoi(m[2])
		if err != nil {
			return Name{}, fmt.Errorf("%q: %w", s, err)
		}
		if n.Ordinal < 2 {
			return Name{}, fmt.Errorf("%q: ordinal below 2", s)
		}
	}
	return n, nil
}

// ValidateTag reports whether a tag may be attached to a snapshot name.
func ValidateTag(tag string) error {
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("tag %q: want letters, digits, dot, dash or underscore, starting with a letter or digit", tag)
	}
	return nil
}

func (n Name) String() string {
	s := n.Time.UTC().Format(TimeLayout)
	if n.Ordinal != 0 {
		s += "." + strconv.Itoa(n.Ordinal)
	}
	if n.Tag != "" {
		s += "-" + n.Tag
	}
	return s
}

// Same reports whether two names are of one snapshot, whatever tag each
// carries: a tag can come and go, but the time and ordinal never change.
func (n Name) Same(o Name) bool {
	return n.Time.Equal(o.Time) && n.Ordinal == o.Ordinal
}

// Untagged is the name without its tag.
func (n Name) Untagged() Name {
	n.Tag = ""
	return n
}

// Protected reports whether pruning must leave the snapshot alone.
func (n Name) Protected() bool { return n.Tag != "" }

// Before orders snapshots oldest first. Timestamp and ordinal together are
// enough to order snapshots gbsnap created, and the tag settles any remaining
// tie so that the order is total whatever a pool turns out to hold.
func (n Name) Before(o Name) bool {
	if !n.Time.Equal(o.Time) {
		return n.Time.Before(o.Time)
	}
	if n.Ordinal != o.Ordinal {
		return n.Ordinal < o.Ordinal
	}
	return n.Tag < o.Tag
}

// NextName picks the name for a snapshot taken at now. It sorts after every
// name the pool already holds for that second, rather than merely differing
// from them: a gap left by pruning must not be filled, because a name sorting
// before the pool's latest would never be replicated — only what follows the
// newest snapshot a target shares is ever sent.
func NextName(now time.Time, existing []Name, tag string) Name {
	n := Name{Time: now.UTC().Truncate(time.Second), Tag: tag}
	for _, e := range existing {
		if !e.Time.Equal(n.Time) {
			continue
		}
		// The plain timestamp is preferred; an ordinal only appears once a
		// second genuinely holds more than one snapshot, and so starts at 2.
		next := max(e.Ordinal+1, 2)
		if next > n.Ordinal {
			n.Ordinal = next
		}
	}
	return n
}
