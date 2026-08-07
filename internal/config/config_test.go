package config

import (
	"strings"
	"testing"

	"github.com/greendrake/gbsnap/internal/run"
)

const full = `
defaults:
  retention:
    pool:   { min: 3 }
    target: { min: 5 }
  sudo: auto

volumes:
  home:
    subvolume: /home
    pool: /pool/snap/home
    targets:
      - nas:/backup/snap/home
      - user@offsite:/backup/home

  etc:
    subvolume: /etc
    pool: /pool/snap/etc
    retention:
      pool: { min: 10 }

  mail:
    pool: /pool/snap/mail
    targets: [nas:/backup/snap/mail]
    sudo: never
`

func TestParse(t *testing.T) {
	cfg, err := parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}

	// Volumes keep the order they were written in, so a run works through them
	// predictably.
	var order []string
	for _, v := range cfg.Volumes {
		order = append(order, v.Name)
	}
	if got := strings.Join(order, " "); got != "home etc mail" {
		t.Fatalf("order = %q", got)
	}

	home := cfg.Volumes[0]
	if home.Subvolume == nil || home.Subvolume.String() != "/home" {
		t.Errorf("home subvolume = %v", home.Subvolume)
	}
	if home.Pool.String() != "/pool/snap/home" {
		t.Errorf("home pool = %v", home.Pool)
	}
	if len(home.Targets) != 2 || home.Targets[1].Host != "user@offsite" {
		t.Errorf("home targets = %v", home.Targets)
	}
	if home.Retention.Pool.Min != 3 || home.Retention.Target.Min != 5 {
		t.Errorf("home retention = %+v", home.Retention)
	}
	if home.Sudo != run.SudoAuto {
		t.Errorf("home sudo = %v", home.Sudo)
	}

	etc := cfg.Volumes[1]
	if etc.Retention.Pool.Min != 10 {
		t.Errorf("etc should override the default pool retention, got %d", etc.Retention.Pool.Min)
	}
	if etc.Retention.Target.Min != 5 {
		t.Errorf("etc should inherit the default target retention, got %d", etc.Retention.Target.Min)
	}
	if len(etc.Targets) != 0 {
		t.Errorf("etc has no targets, got %v", etc.Targets)
	}

	mail := cfg.Volumes[2]
	if mail.Subvolume != nil {
		t.Errorf("mail has no subvolume, got %v", mail.Subvolume)
	}
	if mail.Sudo != run.SudoNever {
		t.Errorf("mail sudo = %v", mail.Sudo)
	}
}

func TestParseBuiltInDefaults(t *testing.T) {
	cfg, err := parse([]byte("volumes:\n  etc:\n    pool: /pool/snap/etc\n"))
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Volumes[0]
	if v.Retention.Pool.Min != DefaultMin || v.Retention.Target.Min != DefaultMin {
		t.Errorf("retention = %+v, want %d on both sides", v.Retention, DefaultMin)
	}
	if v.Sudo != run.SudoAuto {
		t.Errorf("sudo = %v", v.Sudo)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no volumes at all": {
			"defaults:\n  sudo: auto\n", "volumes",
		},
		"empty volumes block": {
			"volumes:\n", "volumes",
		},
		"volume without a pool": {
			"volumes:\n  home:\n    subvolume: /home\n", "no pool",
		},
		"misspelt key": {
			"volumes:\n  home:\n    pool: /p\n    targests: [x]\n", "targests",
		},
		"misspelt key in the defaults": {
			"defaults:\n  retention:\n    poool: { min: 2 }\nvolumes:\n  home:\n    pool: /p\n", "poool",
		},
		"paths in the defaults": {
			"defaults:\n  pool: /p\nvolumes:\n  home:\n    pool: /p\n", "belong to a volume",
		},
		"subvolume on another host": {
			"volumes:\n  home:\n    subvolume: nas:/home\n    pool: /pool/snap/home\n", "different hosts",
		},
		"pool on another host": {
			"volumes:\n  home:\n    subvolume: /home\n    pool: nas:/pool/snap/home\n", "different hosts",
		},
		// One machine, but the two spellings address ssh differently, so gbsnap
		// cannot take them for the same host.
		"one host spelled two ways": {
			"volumes:\n  home:\n    subvolume: user@nas:/home\n    pool: nas:/pool/snap/home\n", "spelled alike",
		},
		"target is the pool": {
			"volumes:\n  home:\n    pool: /pool/snap/home\n    targets: [/pool/snap/home/]\n", "the pool itself",
		},
		"target listed twice": {
			"volumes:\n  home:\n    pool: /p\n    targets: [nas:/b, nas:/b]\n", "listed twice",
		},
		"retention of zero": {
			"volumes:\n  home:\n    pool: /p\n    retention:\n      pool: { min: 0 }\n", "at least 1",
		},
		"negative retention": {
			"volumes:\n  home:\n    pool: /p\n    retention:\n      target: { min: -1 }\n", "at least 1",
		},
		"unknown sudo mode": {
			"volumes:\n  home:\n    pool: /p\n    sudo: perhaps\n", "auto, always or never",
		},
		"empty pool path": {
			"volumes:\n  home:\n    pool: \"nas:\"\n", "empty path",
		},
		"volume named twice": {
			"volumes:\n  home:\n    pool: /a\n  home:\n    pool: /b\n", "",
		},
		"name with a slash": {
			"volumes:\n  a/b:\n    pool: /p\n", `"a/b"`,
		},
		"name with a colon": {
			"volumes:\n  \"a:b\":\n    pool: /p\n", `"a:b"`,
		},
		"name starting with a dash": {
			"volumes:\n  \"-x\":\n    pool: /p\n", `"-x"`,
		},
	} {
		_, err := parse([]byte(tc.yaml))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should mention %q", name, err, tc.want)
		}
	}
}

// A volume may live entirely on a remote host, so long as the subvolume and the
// pool name that host alike; gbsnap then snapshots over ssh. Its targets are
// under no such constraint and may sit anywhere, the invoking host included.
func TestParseRemoteVolume(t *testing.T) {
	cfg, err := parse([]byte("volumes:\n  vault:\n    subvolume: user@nas:/vault\n" +
		"    pool: user@nas:/pool/snap/vault\n    targets: [/backup/snap/vault]\n"))
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Volumes[0]
	if v.Subvolume.Host != "user@nas" || v.Pool.Host != "user@nas" {
		t.Errorf("hosts = %q and %q, want both user@nas", v.Subvolume.Host, v.Pool.Host)
	}
	if len(v.Targets) != 1 || v.Targets[0].Host != "" {
		t.Errorf("a target may be on the invoking host, got %v", v.Targets)
	}
}

func TestVolumeLookup(t *testing.T) {
	cfg, err := parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = "/etc/gbsnap.yaml"
	if v, err := cfg.Volume("etc"); err != nil || v.Name != "etc" {
		t.Fatalf("Volume(etc) = %v %v", v, err)
	}
	if _, err := cfg.Volume("absent"); err == nil {
		t.Error("expected an error for an unknown volume")
	}
}
