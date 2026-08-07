// Package btrfs wraps the btrfs subcommands gbsnap needs.
package btrfs

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/greendrake/gbsnap/internal/run"
)

// Subvolume is what "btrfs subvolume show" reports about one subvolume.
type Subvolume struct {
	Path         string
	Name         string
	UUID         string
	ParentUUID   string // the subvolume this one was snapshotted from, empty if none
	ReceivedUUID string // the stream UUID this one was received from, empty if none
	Generation   uint64
	ReadOnly     bool
}

// Show reads a subvolume's metadata. It needs root.
func Show(ctx context.Context, r run.Runner, path string) (Subvolume, error) {
	out, err := r.Output(ctx, "btrfs", "subvolume", "show", path)
	if err != nil {
		return Subvolume{}, err
	}
	return parseShow(path, out)
}

// unset is what btrfs prints for a field that has no value.
const unset = "-"

func parseShow(path, out string) (Subvolume, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue // the leading path line, and list continuations
		}
		key = strings.TrimSpace(key)
		if key == "Snapshot(s)" {
			break // what follows is a list of paths, not fields
		}
		fields[key] = strings.TrimSpace(value)
	}

	need := func(key string) (string, error) {
		v, ok := fields[key]
		if !ok {
			return "", fmt.Errorf("btrfs subvolume show %s: no %q field in output:\n%s", path, key, out)
		}
		return v, nil
	}
	uuid, err := need("UUID")
	if err != nil {
		return Subvolume{}, err
	}
	flags, err := need("Flags")
	if err != nil {
		return Subvolume{}, err
	}
	genText, err := need("Generation")
	if err != nil {
		return Subvolume{}, err
	}
	gen, err := strconv.ParseUint(genText, 10, 64)
	if err != nil {
		return Subvolume{}, fmt.Errorf("btrfs subvolume show %s: generation %q: %w", path, genText, err)
	}

	blank := func(key string) string {
		if v := fields[key]; v != unset {
			return v
		}
		return ""
	}
	return Subvolume{
		Path:         path,
		Name:         fields["Name"],
		UUID:         uuid,
		ParentUUID:   blank("Parent UUID"),
		ReceivedUUID: blank("Received UUID"),
		Generation:   gen,
		ReadOnly:     strings.Contains(flags, "readonly"),
	}, nil
}

// Snapshot creates a read-only snapshot of src at dst, which must be on the
// same filesystem.
func Snapshot(ctx context.Context, r run.Runner, src, dst string) error {
	return r.Run(ctx, "btrfs", "subvolume", "snapshot", "-r", src, dst)
}

// Delete removes a subvolume.
func Delete(ctx context.Context, r run.Runner, path string) error {
	return r.Run(ctx, "btrfs", "subvolume", "delete", path)
}

// SyncFS commits the pending transactions of the filesystem holding path, so
// that generation numbers reflect every write made so far.
func SyncFS(ctx context.Context, r run.Runner, path string) error {
	return r.Run(ctx, "btrfs", "filesystem", "sync", path)
}

// SendOpts selects what a send command streams.
type SendOpts struct {
	// Parent, when set, streams only the difference from that snapshot.
	Parent string
	// NoData streams metadata alone, which is enough to tell whether anything
	// changed but not to reconstruct the content.
	NoData bool
}

// SendArgv builds the command that streams a snapshot on its own host.
func SendArgv(o SendOpts, path string) []string {
	argv := []string{"btrfs", "send"}
	if o.NoData {
		argv = append(argv, "--no-data")
	}
	if o.Parent != "" {
		argv = append(argv, "-p", o.Parent)
	}
	return append(argv, path)
}

// ReceiveArgv builds the command that writes a stream into a directory.
func ReceiveArgv(dir string) []string {
	return []string{"btrfs", "receive", dir}
}

// DumpArgv builds the command that decodes a stream into readable records
// instead of applying it. It needs neither root nor a btrfs filesystem.
func DumpArgv() []string {
	return []string{"btrfs", "receive", "--dump"}
}

// unchangedRecords are the records that do not amount to a change: the header
// that opens a stream, "subvol" for a full send and "snapshot" for an
// incremental one, and timestamp updates.
var unchangedRecords = map[string]bool{
	"subvol":   true,
	"snapshot": true,
	// Timestamps move whenever a file is merely read, so on their own they do
	// not make a snapshot worth taking.
	"utimes": true,
}

// DumpChanged reports whether a dumped difference describes any real change.
func DumpChanged(dump string) bool {
	for _, line := range strings.Split(dump, "\n") {
		record, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if record == "" {
			continue
		}
		if !unchangedRecords[record] {
			return true
		}
	}
	return false
}
