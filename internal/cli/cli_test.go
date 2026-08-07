package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run invokes gbsnap as the command line would, and returns its exit code and
// everything it wrote.
func runGbsnap(t *testing.T, args ...string) (int, string) {
	t.Helper()
	// Keep lock files out of the invoking user's runtime directory.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var out bytes.Buffer
	code := Main(args, &out, &out)
	return code, out.String()
}

// configFile writes a configuration naming pools under dir, none of which
// exist yet.
func configFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gbsnap.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUsageErrors(t *testing.T) {
	cfg := configFile(t, "volumes:\n  home:\n    pool: /pool/snap/home\n  etc:\n    pool: /pool/snap/etc\n")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no command":        {[]string{}, "usage"},
		"unknown command":   {[]string{"frobnicate"}, "unknown command"},
		"unknown volume":    {[]string{"-c", cfg, "run", "absent"}, `no volume "absent"`},
		"volumes and pools": {[]string{"-c", cfg, "sync", "home", "/pool/x"}, "not a mixture"},
		"sync needs two":    {[]string{"-c", cfg, "sync", "/pool/a"}, "source and a destination"},
		"sync to itself":    {[]string{"sync", "/pool/a", "/pool/a/"}, "same pool"},
		"prune needs min":   {[]string{"prune", "/pool/a"}, "needs -min"},
		"min on a volume":   {[]string{"-c", cfg, "prune", "-min", "2", "home"}, "carry their own retention"},
		"list needs one":    {[]string{"-c", cfg, "list"}, "one volume or pool"},
		"bad tag":           {[]string{"-c", cfg, "snap", "-tag", "a tag"}, "tag"},
		"bad sudo":          {[]string{"-sudo", "perhaps", "status"}, "auto, always or never"},
		"missing config":    {[]string{"-c", "/nonexistent/gbsnap.yaml", "run"}, "no such file"},
	} {
		code, out := runGbsnap(t, tc.args...)
		if code != exitUsage {
			t.Errorf("%s: exit %d, want %d; output:\n%s", name, code, exitUsage, out)
		}
		if !strings.Contains(strings.ToLower(out), strings.ToLower(tc.want)) {
			t.Errorf("%s: output should mention %q, got:\n%s", name, tc.want, out)
		}
	}
}

// The version is asked of gbsnap itself, so it answers without a command and
// without a configuration file.
func TestVersion(t *testing.T) {
	code, out := runGbsnap(t, "-version")
	if code != exitOK {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
	if strings.TrimSpace(out) != "gbsnap "+version {
		t.Errorf("output = %q, want %q", strings.TrimSpace(out), "gbsnap "+version)
	}
}

// Flags may follow the volumes or pools they apply to, as the usage lines read.
func TestFlagsAfterArguments(t *testing.T) {
	pool := filepath.Join(t.TempDir(), "snap")
	for _, name := range []string{"20260807T140000Z", "20260807T140100Z"} {
		if err := os.MkdirAll(filepath.Join(pool, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	code, out := runGbsnap(t, "-sudo", "never", "-n", "prune", pool, "-min", "1")
	if code != exitOK {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "would delete "+pool+"/20260807T140000Z") {
		t.Errorf("output:\n%s", out)
	}

	cfg := configFile(t, "volumes:\n  home:\n    pool: "+pool+"\n")
	code, out = runGbsnap(t, "-c", cfg, "snap", "home", "-tag", "a tag")
	if code != exitUsage || !strings.Contains(out, `tag "a tag"`) {
		t.Errorf("the tag should have been read as a flag; exit %d:\n%s", code, out)
	}
}

// A dry run reports the whole cycle without touching anything, which needs
// neither root nor btrfs.
func TestDryRunCycle(t *testing.T) {
	dir := t.TempDir()
	cfg := configFile(t, "volumes:\n  home:\n    subvolume: "+dir+"/home\n    pool: "+dir+"/snap\n"+
		"    targets: ["+dir+"/backup]\n")

	code, out := runGbsnap(t, "-c", cfg, "-sudo", "never", "--dry-run", "run")
	if code != exitOK {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
	for _, want := range []string{
		"would create pool " + dir + "/snap",
		"would snapshot " + dir + "/home",
		"would create pool " + dir + "/backup",
		"would send",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output should report %q, got:\n%s", want, out)
		}
	}
	if _, err := os.Stat(dir + "/snap"); !os.IsNotExist(err) {
		t.Error("a dry run must not create the pool")
	}
}

func TestDryRunAdHocSync(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "src", "20260807T143205Z"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out := runGbsnap(t, "-sudo", "never", "-n", "sync", dir+"/src", dir+"/dst")
	if code != exitOK {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "would send "+dir+"/src/20260807T143205Z to "+dir+"/dst in full") {
		t.Errorf("output:\n%s", out)
	}
}

// A pool named on the command line has to exist: a mistyped path must not read
// as an empty pool and succeed.
func TestAdHocMissingPool(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	dst := filepath.Join(t.TempDir(), "dst")

	for name, args := range map[string][]string{
		"sync":  {"-sudo", "never", "sync", missing, dst},
		"prune": {"-sudo", "never", "prune", "-min", "1", missing},
		"list":  {"-sudo", "never", "list", missing},
	} {
		code, out := runGbsnap(t, args...)
		if code != exitFailure {
			t.Errorf("%s: exit %d, want %d; output:\n%s", name, code, exitFailure, out)
		}
		if !strings.Contains(out, "does not exist") {
			t.Errorf("%s: output:\n%s", name, out)
		}
	}
}

// A pool holding something gbsnap does not recognise is refused, and the run as
// a whole reports failure.
func TestVolumeFailureExitCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snap", "readme.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := configFile(t, "volumes:\n  home:\n    pool: "+dir+"/snap\n")

	code, out := runGbsnap(t, "-c", cfg, "-sudo", "never", "-n", "run")
	if code != exitFailure {
		t.Errorf("exit %d, want %d; output:\n%s", code, exitFailure, out)
	}
	if !strings.Contains(out, "readme.txt") {
		t.Errorf("output should name the stray entry, got:\n%s", out)
	}
}

// One volume failing to report does not silence the others.
func TestStatusContinuesPastAFailedVolume(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := configFile(t, "volumes:\n  broken:\n    pool: "+bad+"\n  fine:\n    pool: "+dir+"/fine\n")

	code, out := runGbsnap(t, "-c", cfg, "-sudo", "never", "status")
	if code != exitFailure {
		t.Errorf("exit %d, want %d; output:\n%s", code, exitFailure, out)
	}
	if !strings.Contains(out, "notes.txt") {
		t.Errorf("the failure should be reported, got:\n%s", out)
	}
	if !strings.Contains(out, "fine") {
		t.Errorf("the second volume should still be reported, got:\n%s", out)
	}
}

// Global flags work on either side of the command name.
func TestGlobalFlagPlacement(t *testing.T) {
	dir := t.TempDir()
	cfg := configFile(t, "volumes:\n  home:\n    pool: "+dir+"/snap\n")

	for _, args := range [][]string{
		{"-c", cfg, "-sudo", "never", "-n", "-v", "status"},
		{"status", "-c", cfg, "-sudo", "never", "-n", "-v"},
		{"-c", cfg, "status", "-sudo", "never", "-n", "-v"},
	} {
		code, out := runGbsnap(t, args...)
		if code != exitOK {
			t.Errorf("%v: exit %d; output:\n%s", args, code, out)
		}
		// --verbose echoes the commands gbsnap runs.
		if !strings.Contains(out, "+ test -d "+dir+"/snap") {
			t.Errorf("%v: expected the command echo, got:\n%s", args, out)
		}
	}
}

func TestQuietSuppressesActions(t *testing.T) {
	dir := t.TempDir()
	cfg := configFile(t, "volumes:\n  home:\n    subvolume: "+dir+"/home\n    pool: "+dir+"/snap\n")

	code, out := runGbsnap(t, "-c", cfg, "-sudo", "never", "-n", "-q", "run")
	if code != exitOK {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
	if out != "" {
		t.Errorf("a quiet run with nothing to report should say nothing, got:\n%s", out)
	}
}

func TestLockIsExclusive(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withLock("home", func() error {
			close(held)
			// Hold the lock until the second attempt has been made.
			<-done
			return nil
		})
	}()
	<-held

	err := withLock("home", func() error {
		t.Error("the second run should not have got in")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("second attempt gave %v", err)
	}
	done <- nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLockPathIsReadableAndUnique(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	a, err := lockPath("nas:/backup/home")
	if err != nil {
		t.Fatal(err)
	}
	b, err := lockPath("nas:/backup/etc")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("different keys should not share a lock file")
	}
	if !strings.Contains(filepath.Base(a), "nas_") {
		t.Errorf("lock file %q should hint at its key", a)
	}
}
