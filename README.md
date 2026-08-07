# gbsnap

Replicates btrfs snapshots between filesystems, over ssh where they are not on
the same machine. It takes read-only snapshots of live subvolumes, sends them on
incrementally, and retires the old ones.

A single static binary. It needs `btrfs-progs` on both sides, `ssh` for remote
pools, and root, which it takes with `sudo` where it is not already root.

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

Naming no volume works on every volume in the configuration file. An argument
holding a slash or a colon is read as a pool path, anything else as a volume
name, so the two-pool forms need no configuration file at all.

Flags: `-c/-config`, `-n/-dry-run`, `-v/-verbose`, `-q/-quiet`, `-sudo`, and
`-version`. `gbsnap snap` also takes `-tag` and `-force`; `gbsnap prune` takes `-min`.
A flag may come before the command or after it, and after the volumes or pools
it applies to.

Exit status is 0 when all is well, 1 when a volume could not be dealt with, and
2 when the command line or the configuration file is wrong.

## Configuration

Read from `-c`, else `$GBSNAP_CONFIG`, `~/.config/gbsnap.yaml` or `/etc/gbsnap.yaml`.

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

## Retention

`min: n` keeps the newest `n` snapshots of a pool. Two kinds of snapshot are
outside that count and are kept as well as those `n`, not instead of one of
them:

- tagged snapshots, for as long as they carry a tag;
- the newest snapshot a pool and one of its targets both hold, which the next
  incremental send needs as its parent.

Because the parent is pinned by policy rather than by luck, retention can be as
short as you like without ever forcing a full resend.

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

`gbsnap snap -force` skips the question entirely.

## What is checked before trusting a copy

Before sending a difference, the target's copy of the parent must carry the UUID
it would have got by being received from this pool. A copy that does not is
reported as diverged and nothing is sent, because a difference applied to the
wrong history produces a backup that looks fine and is not.

After sending, the snapshot that arrived is checked the same way. Only then may
pruning let go of anything.

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
