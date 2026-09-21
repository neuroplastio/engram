# engram — specification, version 1

An engram channel is three kinds of file under `<base>/<project>/<channel>/`:

    head                     the newest live build — tiny, polled
    journal                  every event since the channel began, oldest first
    builds/<commit>/
      manifest               what one build consists of — immutable, signed
      manifest.sig
      <artifact files>

This document is the contract. The Go package in this repository is its
reference implementation; where they disagree, the package has a bug.

## Grammar

One grammar for all three files: a **strict subset of logfmt**. Every conforming
line parses with any logfmt tool — and with a split on spaces and one on `=`.

- UTF-8, LF line endings. A line is at most 4095 bytes.
- Blank lines and lines starting with `#` are ignored.
- **Line 1 identifies the file**: `engram version=1 kind=journal`. `kind` is
  `journal`, `head` or `manifest`.
  - A reader refuses a `version` it does not know, and a `kind` other than the
    one it asked for.
  - A breaking change ships under a new filename. The old file keeps being
    served.
- Every other line is a **record**: a record-type word, then `key=value` fields
  separated by one or more spaces, in any order.
- A value is a bare token. It runs to the next space and may contain `=`.
  **No spaces, no quotes, no escapes, no control characters.** A value that
  would need them is not allowed. (This is the subset: logfmt proper permits
  quoted values.)
- **Unknown keys and unknown record types are ignored**, and preserved by
  anything that rewrites the file. That is how the format grows.
- A known record missing a required key makes the file invalid.
- **A reader needs no library.** These files are meant to be readable by the
  part of a program that is almost never updated (see [`enboot/`](enboot/)).
  Nothing may be added to the format that a split on spaces and a split on `=`
  cannot read.
- Commits are always the **full 40 characters**. Timestamps are RFC 3339, UTC.

## `journal`

    engram version=1 kind=journal
    channel project=acme name=dev
    publish seq=1 commit=1f3e9aa0… version=26.09.20-dev.1f3e9aa time=2026-09-20T18:02:11Z min_enboot=1
    publish seq=2 commit=77aa01c3… version=26.09.21-dev.77aa01c time=2026-09-21T08:31:40Z min_enboot=1
    scrap seq=3 commit=77aa01c3… time=2026-09-21T08:50:12Z reason=broken
    publish seq=4 commit=a7072af3… version=26.09.21-dev.a7072af time=2026-09-21T09:10:55Z min_enboot=1
    expire seq=5 commit=1f3e9aa0… time=2026-10-21T09:10:55Z

Lines are only ever added, at the end. `seq` starts at 1 and increases by
exactly one per event; a gap makes the journal invalid.

| record | key | |
| --- | --- | --- |
| `channel` | `project`, `name` | what the file claims to be |
| `publish` | `seq` | position in the journal; the same number is signed into the build's manifest |
| | `commit` | the build's identity |
| | `version` | the name people use; never compared, never sorted |
| | `time` | the commit's time |
| | `min_enboot` | lowest enboot protocol the build can be handed to |
| `scrap` | `seq`, `commit`, `time` | the build was **withdrawn**: its files are gone, and a host running it should update |
| | `reason` | optional: `broken`, `security`, `mistake`. An unknown value reads as absent |
| `expire` | `seq`, `commit`, `time` | the build **aged out**: its files are gone; nothing is wrong with it |

**Reading it.** A commit is *live* from its `publish` until a `scrap` or
`expire` naming it. A commit is published at most once; a scrapped or expired
commit never returns. The *newest live build* is the live commit with the
highest `publish` sequence.

## `head`

    engram version=1 kind=head
    channel project=acme name=dev seq=5 updated=2026-10-21T09:10:55Z
    publish seq=4 commit=a7072af3… version=26.09.21-dev.a7072af time=2026-09-21T09:10:55Z min_enboot=1

The `channel` record gains `seq`, the journal's last sequence when the head was
written, and `updated`. The `publish` record is the newest live build, byte for
byte as the journal has it. A channel with no live build has no `publish`
record.

## `manifest`

    engram version=1 kind=manifest
    build project=acme channel=dev seq=4 commit=a7072af3… version=26.09.21-dev.a7072af time=2026-09-21T09:10:55Z min_enboot=1
    artifact name=acme os=linux arch=amd64 size=18300412 sha256=… path=acme_linux_amd64.tar.gz
    artifact name=acme os=darwin arch=arm64 size=17911038 sha256=… path=acme_darwin_arm64.tar.gz

`build` carries the `publish` keys plus `project` and `channel`. An `artifact`
requires all six keys. `path` is a clean relative path inside the build's
directory — no `..`, no leading `/` — so a mirror or a move changes nothing.
The key naming a hash is the algorithm; a second one can sit beside it later.

## Trust

**Only the manifest is signed.** `head` and `journal` just point. They are
rewritten on every publish, and everything in them is advisory until a manifest
confirms it.

`manifest.sig` is an **OpenSSH signature** (`PROTOCOL.sshsig`, the format of
`ssh-keygen -Y sign`) over the manifest's bytes, hash `sha512`, namespace
`engram`, by an Ed25519 key. Anyone can check one with stock tools:

    ssh-keygen -Y verify -f allowed_signers -I <identity> -n engram \
      -s manifest.sig < manifest

A client that has fetched `builds/<commit>/manifest`:

1. verifies `manifest.sig` against the keys **pinned in the client**. The key
   named inside a signature is a hint, never a credential;
2. checks that `project`, `channel` and `commit` in the `build` record are the
   ones it asked for;
3. for an update, requires `seq` to be higher than the highest it has accepted
   on this channel. An explicit rollback by the user skips this check, and only
   this one;
4. checks each artifact's `size` and `sha256` before using it.

What this buys, and what it does not. Whoever can write the channel's storage
can freeze it, hide a real `scrap`, or invent one. They cannot make a client
install anything unsigned, from another project or channel, or older than what
it already has. `scrap` and `expire` are therefore **advice**: a client shows
them to the user and looks for a newer signed build, and never does more.

Rotation is a list. A client pins every key it accepts; a release adds the next
key before the old one retires.

## Publishing

In this order, so that everything a client could be pointed at exists before
anything points at it:

1. Read the journal; the build's `seq` is the next number.
2. Upload the artifacts, then the manifest, then its signature.
3. Append `publish`, and an `expire` for every build past its retention.
4. Write the journal **conditionally** (`If-Match` on the version read in step
   1, or `If-None-Match: *` for a new channel). Losing the race means starting
   again from step 1.
5. Write the head.
6. Delete the files of expired builds, and purge them from any cache in front
   of the store.

**Retention counts from when a build was superseded**, not from when it was
published: a build expires once its successor's `time` is older than the
retention. The newest live build is therefore never expired, and a channel that
goes quiet for a year still has a head that resolves.

Lifetimes are the publisher's to declare, on the objects themselves: `head` and
`journal` with `Cache-Control: max-age=60`; everything under `builds/` as
immutable. Nothing under `builds/<commit>/` ever changes — a replaced release
is a new commit, so a new directory — so a publish never invalidates a cache.

**Removing a build does.** A cache that holds an immutable file goes on serving
it after the file is deleted, and for a build scrapped because it is dangerous
that defeats the scrap. When a build's files go — scrapped or expired — the
publisher purges `builds/<commit>/` from whatever cache is in front of the
store, after deleting them. "Its files are gone" means gone from where clients
fetch them.

## Not in version 1

- **`expires=` on the head** — guards against a stalled channel being passed
  off as current; needs quiet channels re-signed on a schedule, and the head is
  not signed. It would arrive on the manifest or as a separate signed file.
- **Bounding the journal.** It grows without limit. The likely shape is
  segments: an archived `journal.NNNN`, and a new `journal` whose first line
  carries `prev=` and `prev_sha256=` and which repeats the `publish` records of
  every build still live. Old readers ignore both keys.
