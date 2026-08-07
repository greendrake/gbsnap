package btrfs

import (
	"strings"
	"testing"
)

// showSnapshot is verbatim output of btrfs-progs v6.6.3 for a read-only
// snapshot, tabs and all.
const showSnapshot = "S\n" +
	"\tName: \t\t\tS\n" +
	"\tUUID: \t\t\td0064618-d814-9647-a16e-cdc71ac20e13\n" +
	"\tParent UUID: \t\t96141ade-c0f7-2a40-8bab-a67a1b1c1b11\n" +
	"\tReceived UUID: \t\t-\n" +
	"\tCreation time: \t\t2026-08-07 02:34:22 +1200\n" +
	"\tSubvolume ID: \t\t257\n" +
	"\tGeneration: \t\t7\n" +
	"\tGen at creation: \t7\n" +
	"\tParent ID: \t\t5\n" +
	"\tTop level ID: \t\t5\n" +
	"\tFlags: \t\t\treadonly\n" +
	"\tSend transid: \t\t0\n" +
	"\tSend time: \t\t2026-08-07 02:34:22 +1200\n" +
	"\tReceive transid: \t0\n" +
	"\tReceive time: \t\t-\n" +
	"\tSnapshot(s):\n" +
	"\tQuota group:\t\tn/a\n"

// showLive is verbatim output for a writable subvolume that has one snapshot.
const showLive = "V\n" +
	"\tName: \t\t\tV\n" +
	"\tUUID: \t\t\t96141ade-c0f7-2a40-8bab-a67a1b1c1b11\n" +
	"\tParent UUID: \t\t-\n" +
	"\tReceived UUID: \t\t-\n" +
	"\tCreation time: \t\t2026-08-07 02:34:22 +1200\n" +
	"\tSubvolume ID: \t\t256\n" +
	"\tGeneration: \t\t7\n" +
	"\tGen at creation: \t7\n" +
	"\tParent ID: \t\t5\n" +
	"\tTop level ID: \t\t5\n" +
	"\tFlags: \t\t\t-\n" +
	"\tSend transid: \t\t0\n" +
	"\tSend time: \t\t2026-08-07 02:34:22 +1200\n" +
	"\tReceive transid: \t0\n" +
	"\tReceive time: \t\t-\n" +
	"\tSnapshot(s):\n" +
	"\t\t\t\tS\n" +
	"\tQuota group:\t\tn/a\n"

func TestParseShowSnapshot(t *testing.T) {
	got, err := parseShow("/mnt/S", showSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := Subvolume{
		Path:       "/mnt/S",
		Name:       "S",
		UUID:       "d0064618-d814-9647-a16e-cdc71ac20e13",
		ParentUUID: "96141ade-c0f7-2a40-8bab-a67a1b1c1b11",
		Generation: 7,
		ReadOnly:   true,
	}
	if got != want {
		t.Errorf("parseShow =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseShowLive(t *testing.T) {
	got, err := parseShow("/mnt/V", showLive)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentUUID != "" || got.ReceivedUUID != "" {
		t.Errorf("unset fields should be empty, got parent %q received %q", got.ParentUUID, got.ReceivedUUID)
	}
	if got.ReadOnly {
		t.Error("live subvolume should not be read-only")
	}
	if got.UUID != "96141ade-c0f7-2a40-8bab-a67a1b1c1b11" {
		t.Errorf("UUID = %q", got.UUID)
	}
}

// The snapshot list at the end of the output must not be mistaken for fields:
// a subvolume named like a field would otherwise overwrite it.
func TestParseShowIgnoresSnapshotList(t *testing.T) {
	tricky := strings.Replace(showLive, "\t\t\t\tS\n", "\t\t\t\tQuota group: bogus\n", 1)
	got, err := parseShow("/mnt/V", tricky)
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != "96141ade-c0f7-2a40-8bab-a67a1b1c1b11" {
		t.Errorf("UUID = %q", got.UUID)
	}
}

func TestParseShowRejectsIncomplete(t *testing.T) {
	for name, out := range map[string]string{
		"no fields":      "V\n",
		"no UUID":        "V\n\tFlags: \t-\n\tGeneration: \t7\n",
		"no generation":  "V\n\tUUID: \tu\n\tFlags: \t-\n",
		"bad generation": "V\n\tUUID: \tu\n\tFlags: \t-\n\tGeneration: \tlots\n",
	} {
		if _, err := parseShow("/mnt/V", out); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSendArgv(t *testing.T) {
	for _, tc := range []struct {
		opts SendOpts
		want string
	}{
		{SendOpts{}, "btrfs send /pool/x"},
		{SendOpts{Parent: "/pool/w"}, "btrfs send -p /pool/w /pool/x"},
		{SendOpts{Parent: "/pool/w", NoData: true}, "btrfs send --no-data -p /pool/w /pool/x"},
	} {
		if got := strings.Join(SendArgv(tc.opts, "/pool/x"), " "); got != tc.want {
			t.Errorf("SendArgv(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
	if got := strings.Join(ReceiveArgv("/backup"), " "); got != "btrfs receive /backup" {
		t.Errorf("ReceiveArgv = %q", got)
	}
	if got := strings.Join(DumpArgv(), " "); got != "btrfs receive --dump" {
		t.Errorf("DumpArgv = %q", got)
	}
}

// dumpUnchanged is a verbatim incremental dump of a subvolume that only had a
// file read since the parent snapshot.
const dumpUnchanged = `snapshot        ./PROBE                         uuid=2b72a589-79c8-6f4d-90cd-aa3aaf5ad4ca transid=22 parent_uuid=12b30f8a-fc44-0544-81c9-4de835627df4 parent_transid=20
utimes          ./PROBE/plain                   atime=2026-08-07T02:17:39+1200 mtime=2026-08-07T02:17:26+1200 ctime=2026-08-07T02:17:26+1200
`

// dumpFullSend is a verbatim dump of a full send, which opens with subvol.
const dumpFullSend = `subvol          ./S                             uuid=d0064618-d814-9647-a16e-cdc71ac20e13 transid=7
chown           ./S/                            gid=1000 uid=1000
chmod           ./S/                            mode=755
mkfile          ./S/o257-7-0
rename          ./S/o257-7-0                    dest=./S/f
update_extent   ./S/f                           offset=0 len=6
utimes          ./S/f                           atime=2026-08-07T02:34:22+1200 mtime=2026-08-07T02:34:22+1200 ctime=2026-08-07T02:34:22+1200
`

func TestDumpChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		dump string
		want bool
	}{
		"unchanged":       {dumpUnchanged, false},
		"empty":           {"", false},
		"full send":       {dumpFullSend, true},
		"data written":    {dumpUnchanged + "update_extent   ./PROBE/plain    offset=0 len=6\n", true},
		"permissions":     {dumpUnchanged + "chmod           ./PROBE/plain    mode=600\n", true},
		"ownership":       {dumpUnchanged + "chown           ./PROBE/plain    gid=0 uid=0\n", true},
		"extended attrs":  {dumpUnchanged + "set_xattr       ./PROBE/dir      name=user.x\n", true},
		"file removed":    {dumpUnchanged + "unlink          ./PROBE/plain2\n", true},
		"directory added": {dumpUnchanged + "mkdir           ./PROBE/new\n", true},
	} {
		if got := DumpChanged(tc.dump); got != tc.want {
			t.Errorf("%s: DumpChanged = %v, want %v", name, got, tc.want)
		}
	}
}
