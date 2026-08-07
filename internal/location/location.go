// Package location addresses a directory on the invoking host or behind ssh.
package location

import (
	"fmt"
	"path"
	"strings"
)

// Location is a directory, optionally on a remote host reached over ssh.
// The zero value addresses the invoking host with an empty path.
type Location struct {
	Host string // empty means the invoking host; may carry a "user@" prefix
	Path string
}

// Parse reads the [user@]host:path form. A colon only introduces a host when
// it precedes the first slash, so local paths may contain colons.
func Parse(s string) (Location, error) {
	if s == "" {
		return Location{}, fmt.Errorf("empty location")
	}
	if i := strings.IndexAny(s, ":/"); i >= 0 && s[i] == ':' {
		host, p := s[:i], s[i+1:]
		if host == "" {
			return Location{}, fmt.Errorf("location %q: empty host", s)
		}
		if p == "" {
			return Location{}, fmt.Errorf("location %q: empty path", s)
		}
		return Location{Host: host, Path: p}, nil
	}
	return Location{Path: s}, nil
}

// Remote reports whether the location needs an ssh hop.
func (l Location) Remote() bool { return l.Host != "" }

// Child addresses an entry inside the directory.
func (l Location) Child(name string) Location {
	return Location{Host: l.Host, Path: path.Join(l.Path, name)}
}

// Canonical renders the location with its path cleaned, so that spellings of
// the same directory compare and key alike.
func (l Location) Canonical() string {
	return Location{Host: l.Host, Path: path.Clean(l.Path)}.String()
}

// Equal reports whether two locations address the same directory.
func (l Location) Equal(o Location) bool {
	return l.Canonical() == o.Canonical()
}

func (l Location) String() string {
	if l.Host == "" {
		return l.Path
	}
	return l.Host + ":" + l.Path
}
