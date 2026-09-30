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

// IDName is the file in a target pool's directory holding the pool's ID, which
// the records of the pool sending to it know it by: the same pool reached by
// another path or address is still itself, and two disks taking turns at one
// mount point are two. gbsnap writes it the first time it sends to the pool.
// The leading dot keeps it out of the pool listing.
const IDName = ".gbsnap-id"

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// IsID reports whether s is written the way a pool's ID is.
func IsID(s string) bool { return idRe.MatchString(s) }

// NewID makes an ID for a pool. It only has to tell pools apart.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ID reads the pool's ID, "" when it has none yet.
func (p *Pool) ID(ctx context.Context) (string, error) {
	if !p.exists {
		return "", nil
	}
	file := path.Join(p.Loc.Path, IDName)
	there, err := p.runner.Test(ctx, "test", "-f", file)
	if err != nil || !there {
		return "", err
	}
	out, err := p.runner.Output(ctx, "cat", "--", file)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if !IsID(id) {
		return "", fmt.Errorf("%s holds %q, which is not a pool ID", p.Loc.Child(IDName), id)
	}
	return id, nil
}

// SetID gives the pool an ID.
func (p *Pool) SetID(ctx context.Context, id string) error {
	return writeFile(ctx, p.runner, path.Join(p.Loc.Path, IDName), id+"\n")
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
