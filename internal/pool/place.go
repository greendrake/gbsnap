package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/greendrake/gbsnap/internal/run"
)

// PlaceMarker is the file "gbsnap init" leaves in a directory to make it a
// place for pools. It holds the place's ID. Pools are only ever created on the
// receiving side of a send inside a marked place, which is what keeps gbsnap
// from filling the directory a backup disk is mounted on while the disk is not
// there: the marker is on the disk, so the bare mount point is unmarked.
const PlaceMarker = ".gbsnap-place"

var placeIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// IsPlaceID reports whether s is written the way a place's ID is.
func IsPlaceID(s string) bool { return placeIDRe.MatchString(s) }

// NewPlaceID makes the ID for a place being marked. It only has to tell places
// apart, so that a place reached by another path or address is still known as
// itself, and two disks taking turns at one mount point are known apart.
func NewPlaceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ReadPlace returns the ID of the place dir is, or "" when dir is not one.
func ReadPlace(ctx context.Context, r run.Runner, dir string) (string, error) {
	marker := path.Join(dir, PlaceMarker)
	marked, err := r.Test(ctx, "test", "-f", marker)
	if err != nil || !marked {
		return "", err
	}
	out, err := r.Output(ctx, "cat", "--", marker)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if !placeIDRe.MatchString(id) {
		return "", fmt.Errorf("%s holds %q, which is not a place ID", marker, id)
	}
	return id, nil
}

// MarkPlace makes dir, which must be a directory on a btrfs filesystem, a
// place for pools with the given ID.
//
// Marking the wrong directory would bring back the very mistake places exist
// to prevent: a disk that is not mounted leaves a bare directory behind, and a
// marker written there, on the filesystem holding the mount point, would make
// that directory pass for the disk whenever the disk is away. So no directory
// is made, and an empty one is refused unless it is a filesystem or a
// subvolume of its own, which a bare mount point never is; force marks it all
// the same, for a place that really is a plain directory.
func MarkPlace(ctx context.Context, r run.Runner, dir, id string, force bool) error {
	there, err := r.Test(ctx, "test", "-d", dir)
	if err != nil {
		return err
	}
	if !there {
		return fmt.Errorf("%s is not a directory; mount the disk, or make the directory, first", dir)
	}
	// Only btrfs can receive a snapshot, so a place anywhere else could never
	// hold a pool.
	fs, err := r.Output(ctx, "stat", "-f", "-c", "%T", "--", dir)
	if err != nil {
		return err
	}
	if fs := strings.TrimSpace(fs); fs != "btrfs" {
		return fmt.Errorf("%s is on a %s filesystem, not btrfs", dir, fs)
	}
	if !force {
		// A mount point and a subvolume's root both sit on a device of their
		// own, which the directory holding them does not share.
		devs, err := r.Output(ctx, "stat", "-c", "%d", "--", dir, path.Join(dir, ".."))
		if err != nil {
			return err
		}
		entries, err := r.Output(ctx, "ls", "-A", "--", dir)
		if err != nil {
			return err
		}
		d := strings.Fields(devs)
		if len(d) == 2 && d[0] == d[1] && strings.TrimSpace(entries) == "" {
			return fmt.Errorf("%s is an empty directory, neither a mount point nor a subvolume: if a disk mounts "+
				"there, mount it first; if the directory itself is the place, mark it with -force", dir)
		}
	}
	return writeFile(ctx, r, path.Join(dir, PlaceMarker), id+"\n")
}

// writeFile replaces a small file whole: written beside it first, then moved
// over it, so that nothing ever reads it half written.
func writeFile(ctx context.Context, r run.Runner, file, content string) error {
	tmp := file + ".new"
	if err := r.Input(ctx, content, "tee", "--", tmp); err != nil {
		return err
	}
	return r.Run(ctx, "mv", "-T", "--", tmp, file)
}
