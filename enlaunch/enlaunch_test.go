package enlaunch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neuroplastio/engram"
	"github.com/neuroplastio/engram/sshsig"
	"github.com/neuroplastio/engram/store"
)

// The test binary doubles as a launcher (envCfg: Main with this Config, as
// JSON) and as a program a launcher started (envLaunched: print Launched).
const (
	envCfg      = "ENLAUNCH_TEST_CFG"
	envLaunched = "ENLAUNCH_TEST_LAUNCHED"
)

func TestMain(m *testing.M) {
	if s := os.Getenv(envCfg); s != "" {
		var cfg Config
		if err := json.Unmarshal([]byte(s), &cfg); err != nil {
			panic(err)
		}
		Main(cfg)
	}
	if os.Getenv(envLaunched) != "" {
		home, ok := Launched()
		fmt.Printf("launched %s %v env=%s\n", home, ok, os.Getenv(EnvHome))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func commit(c byte) string { return strings.Repeat(string(c), 40) }

// channel is a published channel served over HTTP, and the key it is signed
// with.
type channel struct {
	t       *testing.T
	p       *engram.Publisher
	srv     *httptest.Server
	root    string
	signers string
	n       int
}

func newChannel(t *testing.T) *channel {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := sshsig.Key(priv)
	root := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return &channel{
		t: t, root: root, srv: srv,
		p:       &engram.Publisher{Store: store.Dir(root), Signer: key, Project: "acme", Channel: "dev", Now: func() time.Time { return t0 }},
		signers: sshsig.AllowedSigners("release@example.org", engram.Namespace, key.Public()),
	}
}

// publish publishes a build of commit c: a shell script that says which
// build it is, with what it was started with.
func (ch *channel) publish(c byte) {
	ch.t.Helper()
	ch.n++
	art := filepath.Join(ch.t.TempDir(), "acme")
	script := fmt.Sprintf("#!/bin/sh\necho \"build %s home=$%s fetch=$%s $*\"\n", commit(c)[:7], EnvHome, EnvFetch)
	if err := os.WriteFile(art, []byte(script), 0o755); err != nil {
		ch.t.Fatal(err)
	}
	b := engram.Build{Commit: commit(c), Version: "26.10.02-dev." + string(c), Time: t0.Add(time.Duration(ch.n) * time.Hour), MinEnboot: 1}
	if _, err := ch.p.Publish(context.Background(), b, []engram.Upload{{Name: "acme", OS: runtime.GOOS, Arch: runtime.GOARCH, Local: art}}); err != nil {
		ch.t.Fatal(err)
	}
}

// artifact is where the channel serves a build's program from.
func (ch *channel) artifact(c byte) string {
	return filepath.Join(ch.root, "acme", "dev", "builds", commit(c), "acme")
}

// cfg is a launcher's Config, with a home and a seed of its own.
func (ch *channel) cfg() Config {
	dir := ch.t.TempDir()
	return Config{
		Base: ch.srv.URL, Project: "acme", Channel: "dev", Signers: ch.signers,
		Home: filepath.Join(dir, "home"), Seed: filepath.Join(dir, "seed"),
	}
}

// launch runs the test binary as a launcher with cfg.
func launch(t *testing.T, cfg Config, env []string, args ...string) (string, error) {
	t.Helper()
	b, _ := json.Marshal(cfg)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), envCfg+"="+string(b)), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ran is the line the program printed: the launcher's own lines go first.
func ran(t *testing.T, out string, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

func bin(home string) string {
	target, _ := os.Readlink(filepath.Join(home, "bin"))
	return filepath.Base(target)
}

func TestTheFirstLaunchFetchesThenEveryLaunchHandsOver(t *testing.T) {
	ch := newChannel(t)
	ch.publish('a')
	cfg := ch.cfg()

	out, err := launch(t, cfg, nil, "hello", "there")
	if got, want := ran(t, out, err), "build aaaaaaa home="+cfg.Home+" fetch= hello there"; got != want {
		t.Errorf("first launch ran %q, want %q", got, want)
	}
	if !strings.Contains(out, "acme: fetching the newest build from "+ch.srv.URL+"/acme/dev") {
		t.Errorf("first launch did not say it fetched:\n%s", out)
	}
	if bin(cfg.Home) != commit('a') {
		t.Errorf("bin = %q", bin(cfg.Home))
	}

	// From now on nothing is fetched, or even asked: the server is gone.
	ch.srv.Close()
	out, err = launch(t, cfg, nil, "again")
	if got := ran(t, out, err); got != "build aaaaaaa home="+cfg.Home+" fetch= again" || strings.Contains(out, "fetching") {
		t.Errorf("second launch: %s", out)
	}
}

// What the program installs, by moving bin, is what the next launch runs:
// the launcher compares nothing.
func TestTheLauncherRunsWhatBinPointsAt(t *testing.T) {
	ch := newChannel(t)
	ch.publish('a')
	ch.publish('b')
	cfg := ch.cfg()
	if _, err := Install(context.Background(), cfg, commit('a')); err != nil {
		t.Fatal(err)
	}
	out, err := launch(t, cfg, nil)
	if got := ran(t, out, err); !strings.HasPrefix(got, "build aaaaaaa ") {
		t.Errorf("an older build in bin, the newest on the channel: ran %q", got)
	}
}

func TestFetchReplacesWhatIsInstalled(t *testing.T) {
	ch := newChannel(t)
	ch.publish('a')
	cfg := ch.cfg()
	if _, err := Install(context.Background(), cfg, ""); err != nil {
		t.Fatal(err)
	}
	ch.publish('b')
	out, err := launch(t, cfg, []string{EnvFetch + "=1"}, "x")
	if got := ran(t, out, err); got != "build bbbbbbb home="+cfg.Home+" fetch= x" {
		t.Errorf("%s=1 ran %q", EnvFetch, got)
	}
}

// A package preloads a seed; a launcher runs it until the home has a build.
func TestPreloadFillsASeedAndTheHomeWinsOverIt(t *testing.T) {
	ch := newChannel(t)
	ch.publish('a')
	cfg := ch.cfg()
	out, err := launch(t, cfg, []string{EnvPreload + "=" + cfg.Seed}, "x")
	if err != nil || strings.Contains(out, "build ") || !strings.Contains(out, "26.10.02-dev.a preloaded into "+cfg.Seed) {
		t.Fatalf("preload: %v\n%s", err, out)
	}
	if bin(cfg.Seed) != commit('a') {
		t.Fatalf("seed's bin = %q", bin(cfg.Seed))
	}
	// A seed is root's and run by every user: all of it readable, nothing
	// in it but the build and bin.
	if entries, _ := os.ReadDir(cfg.Seed); len(entries) != 2 {
		t.Errorf("seed holds %v, want builds and bin", entries)
	}
	if st, err := os.Stat(filepath.Join(cfg.Seed, "builds", commit('a'))); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("the build's directory: %v %v", st.Mode(), err)
	}
	if _, err := os.Stat(cfg.Home); err == nil {
		t.Error("preloading made the home")
	}

	ch.publish('b')
	out, err = launch(t, cfg, nil, "x")
	if got := ran(t, out, err); got != "build aaaaaaa home="+cfg.Home+" fetch= x" {
		t.Errorf("the seed's build, with the home to install into: ran %q", got)
	}
	if _, err := Install(context.Background(), cfg, ""); err != nil {
		t.Fatal(err)
	}
	out, err = launch(t, cfg, nil, "x")
	if got := ran(t, out, err); !strings.HasPrefix(got, "build bbbbbbb ") {
		t.Errorf("a build in the home: ran %q", got)
	}
}

func TestALaunchWithNothingToFetchSaysWhy(t *testing.T) {
	ch := newChannel(t)
	out, err := launch(t, ch.cfg(), nil)
	if err == nil || !strings.HasPrefix(out, "acme: fetching") || !strings.Contains(out, "\nacme: ") {
		t.Errorf("an empty channel: %v\n%s", err, out)
	}
}

func TestInstallBelievesOnlyThePinnedKeyAndTheHashes(t *testing.T) {
	ch := newChannel(t)
	ch.publish('a')
	ctx := context.Background()

	other := ch.cfg()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	other.Signers = sshsig.AllowedSigners("x@example.org", engram.Namespace, sshsig.Key(priv).Public())
	if _, err := Install(ctx, other, ""); err == nil {
		t.Error("installed a build signed by a key not pinned")
	}

	data, err := os.ReadFile(ch.artifact('a'))
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] ^= 1 // the size still matches; only the sha256 can tell
	if err := os.WriteFile(ch.artifact('a'), data, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := ch.cfg()
	if _, err := Install(ctx, cfg, ""); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("a tampered artifact: %v", err)
	}
	if bin(cfg.Home) != "." {
		t.Error("bin made")
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.Home, "builds")); len(entries) != 0 {
		t.Errorf("left in builds: %v", entries)
	}
}

func TestInstallKeepsBinAndTheBuildBefore(t *testing.T) {
	ch := newChannel(t)
	for _, c := range []byte("abc") {
		ch.publish(c)
	}
	cfg := ch.cfg()
	builds := func() string {
		entries, _ := os.ReadDir(filepath.Join(cfg.Home, "builds"))
		var got []string
		for _, e := range entries {
			got = append(got, e.Name()[:1])
		}
		sort.Strings(got)
		return strings.Join(got, "")
	}
	for _, c := range []byte("abc") {
		if _, err := Install(context.Background(), cfg, commit(c)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // builds are told apart by when they were linked
	}
	if got := builds(); got != "bc" {
		t.Errorf("after a, b, c: builds %q, want bc", got)
	}
	// Back to b, which is there: nothing fetched, c is now the one before.
	if _, err := Install(context.Background(), cfg, commit('b')); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := Install(context.Background(), cfg, commit('a')); err != nil {
		t.Fatal(err)
	}
	if got := builds(); got != "ab" || bin(cfg.Home) != commit('a') {
		t.Errorf("after b, then a: builds %q, bin %s; want ab, a", got, bin(cfg.Home)[:1])
	}
}

// A program reads the home once, and its children never see it.
func TestLaunchedIsTheHomeTheLauncherSet(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"/home/x/.local/acme", "launched /home/x/.local/acme true env="},
		{"", "launched  false env="},
	} {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), envLaunched+"=1", EnvHome+"="+tc.env)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("%s=%q: %v %q, want %q", EnvHome, tc.env, err, out, tc.want)
		}
	}
}
