package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greendrake/gbsnap/internal/location"
	"github.com/greendrake/gbsnap/internal/run"
)

func write(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("volumes:\n  home:\n    pool: /pool/snap/home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GBSNAP_CONFIG", "")

	underHome := filepath.Join(home, ".config", "gbsnap.yaml")
	fromEnv := filepath.Join(t.TempDir(), "env.yaml")
	explicit := filepath.Join(t.TempDir(), "explicit.yaml")
	one := func(files []string, err error, want string) bool {
		return err == nil && len(files) == 1 && files[0] == want
	}

	// With nothing to find, and nothing installed system wide, the search comes
	// up empty rather than failing.
	if _, err := os.Stat("/etc/gbsnap.yaml"); os.IsNotExist(err) {
		if _, err := os.Stat("/etc/gbsnap.d"); os.IsNotExist(err) {
			got, err := Find("")
			if err != nil || len(got) != 0 {
				t.Errorf("Find with nothing to find = %q, %v", got, err)
			}
		}
	}

	write(t, underHome)
	if got, err := Find(""); !one(got, err, underHome) {
		t.Errorf("Find = %q, %v, want the file under the home directory", got, err)
	}

	// The environment beats the home directory.
	write(t, fromEnv)
	t.Setenv("GBSNAP_CONFIG", fromEnv)
	if got, err := Find(""); !one(got, err, fromEnv) {
		t.Errorf("Find = %q, %v, want the file named by the environment", got, err)
	}

	// An explicit path beats everything.
	write(t, explicit)
	if got, err := Find(explicit); !one(got, err, explicit) {
		t.Errorf("Find = %q, %v, want the file named outright", got, err)
	}

	// An explicit path that is not there is an error, not a silent fallback to
	// one of the others.
	if _, err := Find(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("a missing explicit path should be reported")
	}
}

// The drop-ins beside a configuration file follow it, in name order, and only
// the files that look like YAML count. The drop-ins alone are a configuration,
// with no file beside them.
func TestFindDropIns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GBSNAP_CONFIG", "")

	main := filepath.Join(home, ".config", "gbsnap.yaml")
	dropIns := filepath.Join(home, ".config", "gbsnap.d")
	b := write(t, filepath.Join(dropIns, "b.yaml"))
	a := write(t, filepath.Join(dropIns, "a.yml"))
	write(t, filepath.Join(dropIns, "notes.txt"))
	write(t, filepath.Join(dropIns, ".hidden.yaml"))

	got, err := Find("")
	if err != nil || strings.Join(got, " ") != a+" "+b {
		t.Errorf("Find with drop-ins alone = %q, %v, want %q", got, err, []string{a, b})
	}

	write(t, main)
	got, err = Find("")
	if err != nil || strings.Join(got, " ") != main+" "+a+" "+b {
		t.Errorf("Find = %q, %v, want the file and then its drop-ins", got, err)
	}

	// A file named outright brings its own drop-ins.
	got, err = Find(main)
	if err != nil || len(got) != 3 {
		t.Errorf("Find(%s) = %q, %v", main, got, err)
	}
	if DropIns("/etc/gbsnap.yaml") != "/etc/gbsnap.d" || DropIns("/x/config") != "/x/config.d" {
		t.Errorf("DropIns = %q, %q", DropIns("/etc/gbsnap.yaml"), DropIns("/x/config"))
	}
}

func TestLoad(t *testing.T) {
	path := write(t, filepath.Join(t.TempDir(), "gbsnap.yaml"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q", cfg.Path)
	}
	if len(cfg.Volumes) != 1 || cfg.Volumes[0].Name != "home" {
		t.Errorf("Volumes = %+v", cfg.Volumes)
	}

	// A file that will not parse names itself in the error.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("volumes:\n  home:\n    poool: /x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("Load(bad) = %v, want an error naming the file", err)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

// Pieces are read in order: each may add volumes or set defaults, a later
// piece's defaults winning over an earlier's, and the defaults apply to every
// volume wherever it was written.
func TestLoadPieces(t *testing.T) {
	dir := t.TempDir()
	piece := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mine := piece("mine.yaml", "defaults:\n  targets: [nas:/backup]\n  retention:\n    pool: { min: 2 }\n"+
		"    target: { min: 9 }\n")
	tools := piece("tools.yaml", "volumes:\n  home:\n    subvolume: /home\n    pool: /snap/home\n"+
		"  etc:\n    pool: /snap/etc\n    targets: []\n")
	later := piece("later.yaml", "defaults:\n  retention:\n    pool: { min: 4 }\n")

	cfg, err := Load(mine, tools, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Volumes) != 2 {
		t.Fatalf("Volumes = %+v", cfg.Volumes)
	}
	home, etc := cfg.Volumes[0], cfg.Volumes[1]
	if home.Retention.Pool.Min != 4 || home.Retention.Target.Min != 9 {
		t.Errorf("retention = %+v, want the later piece's pool count and the earlier's target count", home.Retention)
	}
	if len(home.Targets) != 1 || home.Targets[0].String() != "nas:/backup/home" {
		t.Errorf("home targets = %v, want a pool named after it in the place the defaults name", home.Targets)
	}
	if len(etc.Targets) != 0 {
		t.Errorf("etc targets = %v: an empty list of its own should win over the defaults", etc.Targets)
	}

	// Defaults alone are no configuration.
	if _, err := Load(mine); err == nil || !strings.Contains(err.Error(), "no volumes") {
		t.Errorf("Load of defaults alone = %v", err)
	}
	// Nor may two pieces claim one volume.
	if _, err := Load(tools, piece("again.yaml", "volumes:\n  home:\n    pool: /x\n")); err == nil ||
		!strings.Contains(err.Error(), "both") {
		t.Errorf("a volume in two pieces = %v", err)
	}
	// An empty piece is harmless.
	if _, err := Load(piece("empty.yaml", ""), tools); err != nil {
		t.Errorf("an empty piece: %v", err)
	}
}

func TestAdHoc(t *testing.T) {
	poolLoc, err := location.Parse("/pool/snap/home")
	if err != nil {
		t.Fatal(err)
	}
	target, err := location.Parse("nas:/backup/home")
	if err != nil {
		t.Fatal(err)
	}
	v := AdHoc(poolLoc, []location.Location{target}, 5, run.SudoNever)
	if v.Name != "/pool/snap/home" {
		t.Errorf("Name = %q", v.Name)
	}
	if v.Subvolume != nil {
		t.Error("an ad-hoc volume has no subvolume to snapshot")
	}
	if v.Retention.Pool.Min != 5 || v.Retention.Target.Min != 5 {
		t.Errorf("Retention = %+v", v.Retention)
	}
	if len(v.Targets) != 1 || v.Targets[0].String() != "nas:/backup/home" {
		t.Errorf("Targets = %v", v.Targets)
	}
	if !v.AdHoc {
		t.Error("a volume built from the command line should be marked ad hoc")
	}
}
