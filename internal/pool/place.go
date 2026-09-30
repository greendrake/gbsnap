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

// MarkPlace makes dir, which must be on a btrfs filesystem, a place for pools
// with the given ID, creating the directory if need be.
func MarkPlace(ctx context.Context, r run.Runner, dir, id string) error {
	if err := r.Run(ctx, "mkdir", "-p", "--", dir); err != nil {
		return err
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
