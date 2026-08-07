package config

import (
	"os"
	"path/filepath"
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

	// With nothing to find, and nothing installed system wide, the search comes
	// up empty rather than failing.
	if _, err := os.Stat("/etc/gbsnap.yaml"); os.IsNotExist(err) {
		got, err := Find("")
		if err != nil || got != "" {
			t.Errorf("Find with nothing to find = %q, %v", got, err)
		}
	}

	write(t, underHome)
	if got, err := Find(""); err != nil || got != underHome {
		t.Errorf("Find = %q, %v, want the file under the home directory", got, err)
	}

	// The environment beats the home directory.
	write(t, fromEnv)
	t.Setenv("GBSNAP_CONFIG", fromEnv)
	if got, err := Find(""); err != nil || got != fromEnv {
		t.Errorf("Find = %q, %v, want the file named by the environment", got, err)
	}

	// An explicit path beats everything.
	write(t, explicit)
	if got, err := Find(explicit); err != nil || got != explicit {
		t.Errorf("Find = %q, %v, want the file named outright", got, err)
	}

	// An explicit path that is not there is an error, not a silent fallback to
	// one of the others.
	if _, err := Find(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("a missing explicit path should be reported")
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
	if _, err := Load(bad); err == nil {
		t.Fatal("expected an error")
	}

	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("expected an error for a missing file")
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
