# gbsnap

Replicates btrfs snapshots between filesystems, over ssh where they are not on
the same machine. It takes read-only snapshots of live subvolumes, sends them on
incrementally, and retires the old ones.

A single static binary. It needs `btrfs-progs` on both sides, `ssh` for remote
pools, and root, which it takes with `sudo` where it is not already root. A
remote host's `sudo` has no terminal to ask for a password on, so there it has
to need none. The local one may ask once, and gbsnap then keeps it from
forgetting for as long as it runs, however long one send takes.

## Install

Linux, `amd64` and `arm64`. The line below picks the right binary for the
machine it runs on and installs it:

```sh
curl -fsSL "https://github.com/greendrake/gbsnap/releases/latest/download/gbsnap-linux-$(uname -m | sed 's/^x86_64$/amd64/; s/^aarch64$/arm64/')" \
  -o /tmp/gbsnap && sudo install -m 0755 /tmp/gbsnap /usr/local/bin/gbsnap && rm /tmp/gbsnap
```

With `wget` instead:

```sh
wget -qO /tmp/gbsnap "https://github.com/greendrake/gbsnap/releases/latest/download/gbsnap-linux-$(uname -m | sed 's/^x86_64$/amd64/; s/^aarch64$/arm64/')" \
  && sudo install -m 0755 /tmp/gbsnap /usr/local/bin/gbsnap && rm /tmp/gbsnap
```

Every release publishes a `SHA256SUMS` alongside the binaries, and checking
against it before installing is worth the extra line:

```sh
arch=$(uname -m | sed 's/^x86_64$/amd64/; s/^aarch64$/arm64/')
base=https://github.com/greendrake/gbsnap/releases/latest/download
cd "$(mktemp -d)"
curl -fsSL -O "$base/gbsnap-linux-$arch" -O "$base/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
sudo install -m 0755 "gbsnap-linux-$arch" /usr/local/bin/gbsnap
```

`gbsnap -version` reports what is installed. To install elsewhere, replace
`/usr/local/bin` with any directory on `PATH`, and drop the `sudo` if you own
it. Building from source needs nothing but a Go toolchain: see the end of this
file.

## Model

- A **subvolume** is live data, say `/home`.
- A **pool** is a directory of read-only snapshots. Local pools are a path;
  remote ones are written `[user@]host:path`.
- A **volume** names one subvolume, the pool its snapshots go into, and any
  number of **targets** the pool is copied to. Targets are ordinary pools, so
  restoring is replication run the other way.

## Commands

    gbsnap run    [<volume>...]           snapshot, replicate and prune
    gbsnap snap   [<volume>...]           snapshot each volume's subvolume, unless unchanged
    gbsnap sync   [<volume>...]           replicate to every configured target
    gbsnap sync   <src-pool> <dst-pool>   replicate one pool to another
    gbsnap prune  [<volume>...]           retire the snapshots retention no longer covers
    gbsnap prune  <pool> -min <n>         retire all but the newest <n> snapshots of a pool
    gbsnap list   [<volume>|<pool>]       list snapshots
    gbsnap status [<volume>...]           show where each volume stands
    gbsnap restore <volume>               bring a volume's pool back from its first reachable target
    gbsnap untag  <volume> <snapshot>     take a snapshot's tag off, here and at its targets
    gbsnap forget <target>                stop keeping snapshots for a target retired for good
    gbsnap convert [<volume>...]          rename a predecessor's snapshots into gbsnap's naming
    gbsnap convert <pool>...              the same, for pools named outright

Naming no volume works on every volume in the configuration. An argument
holding a slash or a colon is read as a pool path, anything else as a volume
name, so the two-pool forms need no configuration at all.

Flags: `-c/-config`, `-add`, `-n/-dry-run`, `-v/-verbose`, `-q/-quiet`, `-sudo`,
and `-version`. `gbsnap snap` also takes `-tag` and `-force`; `gbsnap run` and
`gbsnap sync` take `-reach`; `gbsnap sync` and `gbsnap restore` take `-snapshot`; `gbsnap prune`
takes `-min` and `-local`. A flag may come before the command or after it, and
after the volumes or pools it applies to.

Exit status is 0 when all is well, 1 when a volume could not be dealt with, and
2 when the command line or the configuration file is wrong.

## Configuration

Read from `-c`, else `$GBSNAP_CONFIG`, `~/.config/gbsnap.yaml` or `/etc/gbsnap.yaml`,
whichever is there first, each together with the drop-ins in the `.d` directory
beside it (`gbsnap.d` beside `gbsnap.yaml`), read after it in name order. The
drop-ins alone make a configuration, with no file beside them. `-add` reads one
more file on top of whatever that finds, and may be given more than once.

Each piece may set defaults, a later piece's winning over an earlier's, and
each may add volumes, which the defaults apply to wherever they were written.
No one piece has to hold volumes, but the whole has to. So a tool can own the
volumes it defines, in a drop-in of its own, and leave the rest to you.

```yaml
defaults:                     # every key can be overridden per volume
  retention:
    pool:   { min: 3 }        # keep the newest 3 snapshots
    target: { min: 3 }
  sudo: auto                  # auto uses sudo only where the login is not root

volumes:
  home:
    subvolume: /home
    pool: /pool/snap/home
    targets:
      - nas:/backup/snap/home
      - user@offsite:/backup/home

  etc:                        # no targets: snapshots are kept locally only
    subvolume: /etc
    pool: /pool/snap/etc

  mail:                       # no subvolume: something else fills this pool,
    pool: /pool/snap/mail     # and gbsnap only replicates and prunes it
    targets: [nas:/backup/snap/mail]

  vault:                      # subvolume and pool both on nas, spelled alike:
    subvolume: nas:/vault     # gbsnap reaches nas over ssh and snapshots there
    pool: nas:/pool/snap/vault
    targets: [/backup/snap/vault]
```

Volumes are worked through in the order they are written. A volume that fails
does not stop the others, and the run as a whole then exits 1.

### Targets in the defaults

In the defaults, `targets` names directories rather than pools: each volume that
has no `targets` of its own gets a pool named after it in each. A volume's own
`targets` win, and `targets: []` gives it none.

```yaml
defaults:
  targets: [backup@nas:/backup]   # home goes to nas:/backup/home, etc to nas:/backup/etc
```

### Volumes by pattern

A volume whose subvolume ends in `/*` stands for every subvolume directly inside
that directory, each a volume of its own, named after its subvolume. Its pool,
and any targets of its own, have a `*` where that name goes; without targets of
its own, each gets a pool named after it in the directories the defaults name. Names
starting with a dot never match, nor does anything that is not a subvolume, nor a
mount of another filesystem, which a snapshot could not leave (it is left out,
saying so: backing it up is its own owner's business), and `exclude` leaves out
the names it lists. A pool outlives its subvolume, so `list`, `status`, `untag`
and `restore` also take the name of a volume a pattern stands for whose
subvolume isn't found; the commands that take snapshots refuse it. The volumes found take the pattern's
place among the others, in name order, and are found afresh at every run, so a
fixed configuration covers subvolumes as they come and go.

```yaml
volumes:
  vms:
    subvolume: /var/lib/vms/*
    pool: /var/lib/vms/.snap/*
    exclude: [scratch]
```

### Where a subvolume and its pool may live

A snapshot can only be taken within one filesystem, so a subvolume and its pool
must be on the same filesystem, and therefore on the same host. Say where that
host is on **both or neither**:

- **neither** — two plain paths, both on the invoking host;
- **both** — the same host prefix on each, which gbsnap reaches over ssh and
  snapshots there, as `vault` does above.

The prefix is compared as it is written, because it is also what addresses the
host to ssh, so `user@nas:` and `nas:` count as two different hosts even when
they are one machine. Naming a host on one of the pair but not the other, or
spelling it two ways, is refused when the configuration is read.

Targets are under no such constraint. Each is an independent pool and may be
anywhere, on any host, since replication is a send and a receive rather than a
snapshot — the `vault` volume above is snapshotted on `nas` and backed up to the
invoking host.

## Snapshot names

A snapshot is named for the UTC second it was taken, `20260807T143205Z`. The
name sorts lexically, carries no colon, and so is safe both in a `host:path` and
in a shell. Two snapshots in the same second are told apart by `.2`, `.3` and so
on.

A name may carry a tag, `20260807T143205Z-keep`. A tagged snapshot is protected:
retention never touches it. Tags come from `gbsnap snap -tag`, they are visible in a
plain `ls`, and they travel with the snapshot when it is replicated.

A tag is a label, not part of what a snapshot is: a snapshot is paired with its
copy by its time and ordinal, whatever tag either carries.

`gbsnap untag <volume> <snapshot>` takes the tag off again, in the pool and at
every target it can reach. A target that cannot be reached has its copy renamed
when it is next reached: the next sync or pruning renames the tagged copies of
snapshots the pool holds untagged, once the UUID check has confirmed each is a
copy, so that pruning can have them there too. Tags only ever come off, so a
restore, where the pool is the backup, puts none back.

## Retention

`min: n` keeps the newest `n` snapshots of a pool. Two kinds of snapshot are
outside that count and are kept as well as those `n`, not instead of one of
them:

- tagged snapshots, for as long as they carry a tag;
- for each target, the newest snapshot the pool and the target both hold, which
  the next incremental send needs as its parent. For a target that cannot be
  reached, the pool's record of it says which that is (see below).

Because the parent is pinned by policy rather than by luck, retention can be as
short as you like without ever forcing a full resend, however long a target is
away.

`gbsnap prune -local` prunes a volume's own pool alone, reaching no target at
all and going by the records.

## Targets as written, and targets out of reach

A target is taken as written. A target pool that is not there is created at the
first send, and what is or is not mounted where it goes is for the caller to see
to: a backup disk that is not mounted when gbsnap runs gets its pool, and a
full send, on whatever filesystem holds the mount point.

A target is **offline** only when ssh cannot reach its host. `gbsnap sync` and
`gbsnap run` skip it and say so, quoting ssh, which fails alike for a host that
is off and for one that turns the login away. The run does not fail for it,
unless `-reach` asked it to reach every target. A destination named outright on
the command line always has to be reached.

The first time gbsnap sends to a target pool, or prunes it, it gives the pool an
ID, in a `.gbsnap-id` file in its directory. After each send to one of a configured volume's targets, the pool
records the newest snapshot the target now shares with it, in a
`.gbsnap-targets` file in the pool's directory, keyed by the target's ID. So a
target reached by another path or address is still known as itself, and two
disks taking turns at one mount point are known apart. Pruning keeps the
snapshot each record names, so the next send to a target is incremental however
long it was out of reach or away. The record is only ever trusted to say what to
keep, never what to build on: that is for the UUID check to settle once the
target is reached. A send between pools named outright, a restore among them,
records nothing: the source of a send is never written to.

`gbsnap status` shows each target's record: what it last received and when,
whether it is offline (with its ID), and any target the pool keeps a snapshot
for that is away: no longer configured, or not the pool at its location now. `gbsnap
forget <target>` drops the record of a target retired for good, so that pruning
stops keeping a snapshot for it. The target is named by its ID, or by where it
was reached, as its pool or the directory holding it; where that names more than
one target, as it does two disks that took turns at one mount point, it is
refused, and only the ID will do.

A target last reached by a version of gbsnap from before records has none, and
its pool no ID, until it is next reached. One last reached by 0.2.0 has a record
keyed another way, which is carried over to the pool's ID when it gets one, if
the pool holds the snapshot the record names.

## How a change is noticed

`gbsnap snap` does not take a snapshot of a subvolume that has not changed. It first
compares generation numbers, having asked the filesystem to commit so that they
are current. Where they agree, nothing has been written since the last snapshot
and there is nothing to do; that check reads only, so a volume nobody touches
stays untouched however often gbsnap looks at it.

A generation that has moved settles nothing, because reading a file moves it
too. Only then does gbsnap take a throwaway snapshot and describe the difference in
full. Anything at all counts as a change — data, permissions, ownership,
extended attributes, a file added or removed — except timestamps on their own.
Should describing it fail, the snapshot is taken regardless, saying why: the
question only decides whether a snapshot is worth taking, and a skipped one would
leave the volume out of its backup.

`gbsnap snap -force` skips the question entirely.

## What is checked before trusting a copy

Before sending a difference, the target's copy of the parent must carry the UUID
it would have got by being received from this pool, or, sent back the other way
as a restore is, be the very snapshot the pool's copy was received from. A copy
that is neither is reported as diverged and nothing is sent, because a
difference applied to the wrong history produces a backup that looks fine and
is not.

After sending, the snapshot that arrived is checked the same way. Only then may
pruning let go of anything.

## Restoring

Replication run the other way is a restore: `gbsnap sync <backup-pool> <pool>`
sends what the backup holds past the newest snapshot the two share, or all of
it when they share none. `-snapshot <name>` sends that one snapshot instead,
however old: how a snapshot the pool has already let go of comes back from a
backup that kept it longer. It goes as a change from the shared snapshot nearest
to it, where there is one.

`gbsnap restore <volume> [-snapshot <name>]` does the same for a configured
volume, from the first of its targets that can be reached and holds anything,
or, with a snapshot named, holds that one. A volume a pattern stands for can be
restored before its subvolume is there, or after it has gone: restore names it
all the same, unless more than one pattern could stand for it.

## Concurrent runs

Each volume is locked on the invoking host for as long as gbsnap is working on it. A
second run that finds the lock held fails and says so rather than waiting. The
lock is keyed by the volume's pool, so a configured run and a pool named on the
command line exclude one another when they are the same pool.

`gbsnap status` takes no lock: it only reads, and has to be able to answer while a
run is under way.

## Dry runs

`-n` reads and checks everything a real run would, reports every change it would
make, and makes none of them: no directory is created, no snapshot taken or
deleted, nothing sent. The one thing it will not do is examine a subvolume for
changes in full, since that would mean writing a throwaway snapshot; it says so
where that is the check it stopped short of.

## Pools from the predecessor

`gbsnap convert` renames the snapshots of a pool this tool's predecessor filled
into the naming above, so that such a pool can be carried on with rather than
discarded. Until a pool is converted every command refuses it, and says so.

The predecessor spelled the date two ways over its life, and both are read:

| Written by the predecessor | Becomes |
|---|---|
| `2026-08-02.0001` | `20260802T000000Z` |
| `2026-08-02.0002` | `20260802T000000Z.2` |
| `2023.06.06.01-pinned` | `20230606T000000Z-pinned` |
| `2023.12.31.11-keep` | `20231231T000000Z.11-keep` |

    gbsnap convert [<volume>...]     # each volume's pool and every one of its targets
    gbsnap convert <pool>...         # pools named outright

A volume's pool and its targets are converted together, because a snapshot is
paired with its copy by name alone: converting one side by itself would leave
the two unable to recognise what they already share, and the next `gbsnap sync`
would start again from nothing.

The predecessor recorded the date and nothing finer, so a converted snapshot
lands on midnight UTC of its date, the sequence number within the date becoming
the ordinal. The order the predecessor kept is preserved, and the tag travels
across. The width the sequence was written to means nothing, so `.01` and
`.0001` are both the first of their day. The new name is worked out from the
old name alone and never from anything the filesystem holds, which is what
makes both sides agree on it; btrfs records the true creation time regardless.

Renaming leaves a snapshot's UUID untouched, so an incremental chain survives
the conversion: the first copy after converting is a difference against the
snapshot the target already holds, not a full resend.

A pool is converted whole or not at all. An entry belonging to neither naming,
a rename that would land on a name the pool already holds, two entries that
would become the same name, or something that is not a read-only snapshot stops
the conversion with nothing renamed. Since only the date matters, one date and
sequence written both ways — `2023-06-06.01` and `2023.06.06.01` — is that last
kind of collision, and is reported rather than letting one displace the other.
Names already in gbsnap's form are left alone, so converting is safe to repeat
and safe to interrupt, and `-n` shows the renames without making them.

This command exists only to carry pools across, and goes when none are left.

## Building and testing

    make build              # a gbsnap binary in this directory
    make lint               # gofmt and go vet, both build tag sets
    make test               # unit tests: no root, no btrfs
    make test-integration   # against real btrfs; needs root or a passwordless sudo
    make dist               # the release binaries, for linux/amd64 and linux/arm64

The integration tests build loopback btrfs filesystems, and the ssh ones bring
up an ssh daemon of their own on the loopback address with a throwaway key, so
they leave the machine's own ssh configuration alone. They skip cleanly where
btrfs, root or sshd is unavailable.

`make lint` and `make test` are what CI runs on every push and pull request, so
a change that passes them locally passes there.

## Releasing

Releases are cut by the same workflow, from a push whose commit message
contains `[release]`, and only once lint and the tests have passed:

1. bump `VERSION` — it holds the number alone, say `0.2.0`;
2. commit with `[release]` somewhere in the message;
3. push to GitHub.

The workflow builds both binaries, tags the commit `v<VERSION>`, and publishes
a release carrying `gbsnap-linux-amd64`, `gbsnap-linux-arm64` and `SHA256SUMS`, with
notes generated from the commits since the previous tag. Asking for a release
whose version already has one fails rather than overwriting it, so bumping
`VERSION` is what makes a release, and `[release]` only says when.

The binaries are stamped with the version, which `gbsnap -version` prints. A binary
built any other way reports `dev`.

## License

MIT — see [LICENSE](LICENSE).
