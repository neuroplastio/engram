package enlaunch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/neuroplastio/engram"
)

// Install puts a build into the home and points bin at it: the build of
// commit (a full one), or the channel's newest when commit is empty. The
// manifest must be signed by one of the pinned keys and every artifact must
// match it; a build already in the home is not fetched again. Install never
// compares builds — which one to run is the caller's choice — and keeps bin's
// build and the one installed before it.
func Install(ctx context.Context, cfg Config, commit string) (*engram.Manifest, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	c, err := cfg.client()
	if err != nil {
		return nil, err
	}
	var m *engram.Manifest
	if commit == "" {
		if m, err = c.Latest(ctx, 0); err == nil && m == nil {
			err = fmt.Errorf("enlaunch: %s holds no build", cfg.url())
		}
	} else {
		m, err = c.Manifest(ctx, commit)
	}
	if err != nil {
		return nil, err
	}

	builds := filepath.Join(cfg.Home, "builds")
	if err := os.MkdirAll(builds, 0o755); err != nil {
		return nil, err
	}
	unlock, err := lock(builds)
	if err != nil {
		return nil, err
	}
	defer unlock()

	dir := filepath.Join(builds, m.Build.Commit)
	if !complete(dir, cfg.Names) {
		tmp, err := os.MkdirTemp(builds, ".new-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		// Readable by all: a seed is root's, and run by every user.
		if err := os.Chmod(tmp, 0o755); err != nil {
			return nil, err
		}
		for _, name := range cfg.Names {
			a, ok := m.Find(name, runtime.GOOS, runtime.GOARCH)
			if !ok {
				return nil, fmt.Errorf("enlaunch: %s has no %s for %s/%s", m.Build.Version, name, runtime.GOOS, runtime.GOARCH)
			}
			b, err := c.Download(ctx, m, a)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(tmp, name), b, 0o755); err != nil {
				return nil, err
			}
		}
		os.RemoveAll(dir) // what an interrupted install left
		if err := os.Rename(tmp, dir); err != nil {
			return nil, err
		}
	}
	// Its time is when it was last linked: prune keeps the newest one.
	now := time.Now()
	os.Chtimes(dir, now, now)
	if err := link(cfg.Home, m.Build.Commit); err != nil {
		return nil, err
	}
	prune(cfg.Home)
	return m, nil
}

func complete(dir string, names []string) bool {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// link points bin at a build, atomically: a launch sees the old build or
// the new one.
func link(home, commit string) error {
	tmp := filepath.Join(home, ".bin-new")
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("builds", commit), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(home, "bin"))
}

// prune keeps bin's build and the one linked before it, to go back to, and
// removes the rest. A build that is running keeps running: its files go when
// it exits.
func prune(home string) {
	builds := filepath.Join(home, "builds")
	entries, err := os.ReadDir(builds)
	if err != nil {
		return
	}
	keep := ""
	if target, err := os.Readlink(filepath.Join(home, "bin")); err == nil {
		keep = filepath.Base(target)
	}
	type build struct {
		name string
		at   time.Time
	}
	var others []build
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			continue
		}
		switch name := e.Name(); {
		case strings.HasPrefix(name, ".new-"):
			if time.Since(st.ModTime()) > time.Hour {
				os.RemoveAll(filepath.Join(builds, name)) // left by an install that died
			}
		case name != keep && len(name) == 40:
			others = append(others, build{name, st.ModTime()})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].at.After(others[j].at) })
	for i, b := range others {
		if i > 0 {
			os.RemoveAll(filepath.Join(builds, b.name))
		}
	}
}
