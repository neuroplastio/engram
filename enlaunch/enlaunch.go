// Package enlaunch is what a program's launcher is built from: a thin binary,
// with the program's channel and key compiled in, that a package manager can
// own (/usr/bin/<program>) while the program itself — the complete binary —
// lives in a home it can update:
//
//	~/.local/<project>/
//	  builds/<commit>/   one build: its artifacts by name
//	  bin                a symlink to builds/<commit>: the build to run
//
// The launcher does one thing on its own: when no build is installed it
// fetches the channel's newest, verified, and links it. Otherwise it hands
// over at once — execve, same pid, same arguments — and never compares,
// checks or updates anything. Updating is the program's: its own update
// command calls Install, which puts a build into the home and moves bin, and
// the next launch runs it. The program knows a launcher started it, and where
// the home is, from Launched.
//
// A launcher is opt-in. A program that ships without one is a complete
// binary that works as it always did; enlaunch is not in its start.
package enlaunch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neuroplastio/engram"
	"github.com/neuroplastio/engram/sshsig"
)

// The environment between a launcher and its program.
const (
	// EnvHome is set on the program a launcher starts: the home to install
	// into. Read with Launched.
	EnvHome = "ENLAUNCH_HOME"
	// EnvFetch, set to anything, has the launcher fetch the channel's newest
	// build into the home before it hands over, whatever is installed: the
	// way back from a broken build.
	EnvFetch = "ENLAUNCH_FETCH"
	// EnvPreload names a directory the launcher fetches the newest build into,
	// laid out as a home, and then exits without starting anything: how a
	// package carries a build (ENLAUNCH_PRELOAD=$pkgdir/usr/lib/<project>).
	EnvPreload = "ENLAUNCH_PRELOAD"
)

// fetchTimeout bounds a launcher's fetch: a manifest and a binary of a few
// tens of megabytes.
const fetchTimeout = 10 * time.Minute

// Config is one program on one channel.
type Config struct {
	// Base, Project and Channel name the channel: <Base>/<Project>/<Channel>.
	Base, Project, Channel string
	// Signers are the keys a manifest must be signed by, as allowed_signers
	// lines (ssh-keygen -Y verify). Pinned in the binary, never fetched.
	Signers string
	// Names are the artifacts a build is installed with; the first is the
	// program. Default: Project.
	Names []string
	// Home holds the builds. Default: ~/.local/<Project>.
	Home string
	// Seed is a home a package filled (EnvPreload), read-only: the launcher
	// runs its build while Home has none. Default: /usr/lib/<Project>.
	Seed string
}

func (c Config) withDefaults() (Config, error) {
	if c.Project == "" {
		return c, errors.New("enlaunch: no project")
	}
	if len(c.Names) == 0 {
		c.Names = []string{c.Project}
	}
	if c.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, err
		}
		c.Home = filepath.Join(home, ".local", c.Project)
	}
	if c.Seed == "" {
		c.Seed = filepath.Join("/usr/lib", c.Project)
	}
	return c, nil
}

func (c Config) client() (*engram.Client, error) {
	keys, err := sshsig.ParseAllowedSigners([]byte(c.Signers))
	if err == nil && len(keys) == 0 {
		err = errors.New("no keys")
	}
	if err != nil {
		return nil, fmt.Errorf("enlaunch: signers: %w", err)
	}
	return &engram.Client{Base: c.Base, Project: c.Project, Channel: c.Channel, Keys: keys}, nil
}

func (c Config) url() string {
	return strings.TrimRight(c.Base, "/") + "/" + c.Project + "/" + c.Channel
}

// launchedHome is EnvHome as this process started with it. It is removed
// from the environment, so the program's own children never see it.
var launchedHome = func() string {
	h := os.Getenv(EnvHome)
	os.Unsetenv(EnvHome)
	return h
}()

// Launched is the home of the launcher that started this program, and
// whether one did. A program's update command installs there, with Install,
// instead of replacing its own binary.
func Launched() (home string, ok bool) {
	return launchedHome, launchedHome != ""
}

// Main is a launcher's whole main: it runs the program installed in the
// home, or the seed's, fetching the channel's newest first when there is
// neither. It returns only by exiting.
func Main(cfg Config) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		fail(cfg, err)
	}
	preload, fetch := os.Getenv(EnvPreload), os.Getenv(EnvFetch) != ""
	os.Unsetenv(EnvPreload)
	os.Unsetenv(EnvFetch)
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	if preload != "" {
		into := cfg
		into.Home = preload
		m, err := Install(ctx, into, "")
		if err != nil {
			fail(cfg, err)
		}
		fmt.Fprintf(os.Stderr, "%s: %s preloaded into %s\n", cfg.Project, m.Build.Version, preload)
		os.Exit(0)
	}

	path, ok := linked(cfg.Home, cfg.Names[0])
	if !ok && !fetch {
		path, ok = linked(cfg.Seed, cfg.Names[0])
	}
	if fetch || !ok {
		fmt.Fprintf(os.Stderr, "%s: fetching the newest build from %s\n", cfg.Project, cfg.url())
		m, err := Install(ctx, cfg, "")
		if err != nil {
			fail(cfg, err)
		}
		if path, ok = linked(cfg.Home, cfg.Names[0]); !ok {
			fail(cfg, fmt.Errorf("enlaunch: %s is installed but %s is not there", m.Build.Version, filepath.Join(cfg.Home, "bin", cfg.Names[0])))
		}
		fmt.Fprintf(os.Stderr, "%s: %s installed in %s\n", cfg.Project, m.Build.Version, cfg.Home)
	}
	cancel()
	os.Setenv(EnvHome, cfg.Home)
	fail(cfg, run(path, os.Args, os.Environ()))
}

// linked is the program in a home's bin, when it is there to run.
func linked(home, name string) (string, bool) {
	path := filepath.Join(home, "bin", name)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return path, true
}

func fail(cfg Config, err error) {
	name := cfg.Project
	if name == "" {
		name = "enlaunch"
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
	os.Exit(1)
}
