package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// withLock holds an exclusive lock on the invoking host for the length of the
// work, so that two runs cannot interleave snapshots or pruning on the same
// volume. A run that finds the lock held fails rather than waits: the one
// already under way is doing the work anyway.
func withLock(key string, work func() error) error {
	path, err := lockPath(key)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another gbsnap already holds %s", path)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return work()
}

func lockPath(key string) (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir != "" {
		dir = filepath.Join(dir, "gbsnap")
	} else {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("gbsnap-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The readable part is for whoever looks in the directory; the digest is
	// what actually keeps two keys apart.
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, readable(key)+"-"+hex.EncodeToString(sum[:6])+".lock"), nil
}

// readable reduces a key to something a file name can carry.
func readable(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "pool"
	}
	return b.String()
}
