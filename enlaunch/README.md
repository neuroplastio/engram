# enlaunch

What a program's launcher is built from: a thin binary, with the program's
channel and key compiled in, that a package manager can own while the program
itself — the complete binary — lives in a home where it can update itself.

```go
// cmd/acme-launcher: installed as /usr/bin/acme by a package
package main

import "github.com/neuroplastio/engram/enlaunch"

func main() {
	enlaunch.Main(enlaunch.Config{
		Base:    "https://pkg.example.com",
		Project: "acme",
		Channel: "dev",
		Signers: signers, // allowed_signers lines, pinned in the binary
	})
}
```

```
~/.local/acme/
  builds/<commit>/   one build: its artifacts by name
  bin                a symlink to builds/<commit>: the build to run
```

**The launcher bootstraps, then gets out of the way.** When `bin` is there it
hands over at once — `execve`: same pid, same arguments, same terminal — and
never compares, checks or updates anything. When there is no build yet it
fetches the channel's newest, checks the manifest against the pinned keys and
every artifact against the manifest, links it, and hands over.

**Updating is the program's.** The launcher sets `ENLAUNCH_HOME` on the
program it starts; the program's update command reads it with
`enlaunch.Launched()` and calls `enlaunch.Install(ctx, cfg, commit)`, which
installs that build into the home and moves `bin`. The next launch runs it.
Which build to install — the newest, never an older one unless asked — is the
program's call. The home keeps `bin`'s build and the one before it.

```go
if home, ok := enlaunch.Launched(); ok {
	cfg.Home = home
	_, err = enlaunch.Install(ctx, cfg, m.Build.Commit) // the build the program chose
} else {
	// not started by a launcher: the program updates the way it always did
}
```

**A launcher is opt-in.** A program that never ships one is a complete binary
that works as before; nothing of enlaunch runs at its start.

## Packages

A package installs the launcher as the program's name. It can also carry a
build, so the first launch needs no network:

```sh
# in a PKGBUILD's package(): fetch the newest build into a seed, and nothing else
ENLAUNCH_PRELOAD="$pkgdir/usr/lib/acme" ./acme-launcher
```

The launcher runs the seed's build (`/usr/lib/<project>` by default) while the
user's home has none, still with `ENLAUNCH_HOME` set to the home, so the
program's first update starts it. After that, the home's build is what runs.

## The environment

| | |
| --- | --- |
| `ENLAUNCH_HOME` | set by the launcher on the program: the home to install into |
| `ENLAUNCH_FETCH=1` | fetch the channel's newest into the home before handing over, whatever is installed — the way back from a broken build |
| `ENLAUNCH_PRELOAD=<dir>` | fetch the newest into `<dir>`, laid out as a home, and exit |

It is its own module, so that a launcher takes on the engram library and
nothing else:

```
go get github.com/neuroplastio/engram/enlaunch
```
