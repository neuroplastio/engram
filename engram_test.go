package engram

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroplastio/engram/sshsig"
	"github.com/neuroplastio/engram/store"
)

func commit(c byte) string { return strings.Repeat(string(c), 40) }

func TestParseRefusesWhatTheGrammarCannotCarry(t *testing.T) {
	for name, in := range map[string]string{
		"quotes":        "engram version=1 kind=head\nchannel project=\"acme\" name=dev seq=1\n",
		"tab":           "engram version=1 kind=head\nchannel\tproject=acme\n",
		"bare token":    "engram version=1 kind=head\nchannel project=acme stray\n",
		"wrong kind":    "engram version=1 kind=journal\n",
		"wrong version": "engram version=2 kind=head\n",
		"not engram":    "acme-log version=1 kind=head\n",
		"empty":         "",
	} {
		if _, err := Parse(strings.NewReader(in), KindHead); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := Parse(strings.NewReader("engram version=2 kind=head\n"), KindHead); !errors.Is(err, ErrVersion) {
		t.Errorf("a newer major version should be ErrVersion, got %v", err)
	}
}

func TestParseIgnoresWhatItDoesNotKnow(t *testing.T) {
	in := "# a comment\n\nengram version=1 kind=journal future=yes\n" +
		"channel project=acme name=dev color=blue\n" +
		"hologram seq=1 anything=goes\n" +
		"publish   seq=1 commit=" + commit('a') + "  version=v1 time=2026-09-21T09:00:00Z min_enboot=1 extra=1\n"
	j, err := ParseJournal([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := j.Newest(); !ok || b.Commit != commit('a') {
		t.Fatalf("newest = %+v, %v", b, ok)
	}
	// Appending must not disturb what was there, unknown records included.
	if err := j.Scrap(commit('a'), ReasonBroken, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	out, _ := j.Encode()
	if !strings.Contains(string(out), "hologram seq=1 anything=goes\n") || !strings.Contains(string(out), "color=blue") {
		t.Errorf("unknown content was lost:\n%s", out)
	}
}

func TestJournalFold(t *testing.T) {
	at := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	j := NewJournal("acme", "dev")
	for _, c := range []byte{'a', 'b', 'c'} {
		if _, err := j.Publish(Build{Commit: commit(c), Version: "v", Time: at, MinEnboot: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Scrap(commit('c'), ReasonSecurity, at); err != nil {
		t.Fatal(err)
	}
	if err := j.Expire(commit('a'), at); err != nil {
		t.Fatal(err)
	}

	data, err := j.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseJournal(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if b, _ := back.Newest(); b.Commit != commit('b') || b.Seq != 2 {
		t.Errorf("newest live = %+v, want b at seq 2 (c was scrapped)", b)
	}
	if back.LastSeq() != 5 {
		t.Errorf("last seq = %d, want 5", back.LastSeq())
	}
	if s, r := back.Lookup(commit('c')); s != "scrap" || r != ReasonSecurity {
		t.Errorf("lookup c = %q %q", s, r)
	}
	if s, _ := back.Lookup(commit('a')); s != "expire" {
		t.Errorf("lookup a = %q", s)
	}
	if _, err := back.Publish(Build{Commit: commit('c'), Version: "v", Time: at}); !errors.Is(err, ErrPublished) {
		t.Errorf("a scrapped commit came back: %v", err)
	}

	// The head repeats the journal's line byte for byte.
	head, _ := back.Head(at)
	var line string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "publish seq=2 ") {
			line = l
		}
	}
	if !strings.Contains(string(head), line+"\n") {
		t.Errorf("head does not repeat the journal's record:\n%s", head)
	}

	if _, err := ParseJournal([]byte(strings.Replace(string(data), "seq=2", "seq=7", 1))); err == nil {
		t.Error("a gap in the sequence parsed")
	}
}

func TestManifestPathsStayInside(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", "a/../../x", "", "./x"} {
		m := &Manifest{Project: "acme", Channel: "dev", Build: Build{Seq: 1, Commit: commit('a'), Version: "v"},
			Artifacts: []Artifact{{Name: "acme", OS: "linux", Arch: "amd64", SHA256: strings.Repeat("0", 64), Path: p}}}
		if _, err := m.Encode(); err == nil {
			t.Errorf("path %q encoded", p)
		}
	}
}

// publishAndServe publishes two builds into a directory and serves it.
func publishAndServe(t *testing.T) (*Publisher, *Client, string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := sshsig.Key(priv)
	root := t.TempDir()

	art := filepath.Join(t.TempDir(), "acme_linux_amd64.tar.gz")
	os.WriteFile(art, []byte("not really a tarball"), 0o644)

	now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	p := &Publisher{Store: store.Dir(root), Signer: key, Project: "acme", Channel: "dev", Now: func() time.Time { return now }}
	for _, c := range []byte{'a', 'b'} {
		_, err := p.Publish(context.Background(), Build{Commit: commit(c), Version: "26.09.21-dev." + string(c), Time: now, MinEnboot: 1},
			[]Upload{{Name: "acme", OS: "linux", Arch: "amd64", Local: art}})
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return p, &Client{Base: srv.URL, Project: "acme", Channel: "dev", Keys: []ed25519.PublicKey{key.Public()}}, root
}

func TestPublishThenUpdate(t *testing.T) {
	ctx := context.Background()
	p, c, root := publishAndServe(t)

	m, err := c.Latest(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Build.Commit != commit('b') || m.Build.Seq != 2 {
		t.Fatalf("latest = %+v", m.Build)
	}
	a, ok := m.Find("acme", "linux", "amd64")
	if !ok {
		t.Fatal("no linux/amd64 artifact")
	}
	if data, err := c.Download(ctx, m, a); err != nil || string(data) != "not really a tarball" {
		t.Fatalf("download: %q %v", data, err)
	}
	if m, err := c.Latest(ctx, 2); m != nil || err != nil {
		t.Errorf("already at 2: got %v %v", m, err)
	}

	// The airdrop: my commit, no index.
	if m, err := c.Manifest(ctx, commit('a')); err != nil || m.Build.Seq != 1 {
		t.Errorf("manifest of a: %v %v", m, err)
	}
	if _, err := c.Manifest(ctx, commit('f')); !errors.Is(err, ErrNotLive) {
		t.Errorf("unknown commit: %v", err)
	}

	// Re-running a publish is recognisable, so a workflow can call it success.
	if _, err := p.Publish(ctx, Build{Commit: commit('b'), Version: "v", Time: p.now()}, []Upload{{Local: "/dev/null"}}); !errors.Is(err, ErrPublished) {
		t.Errorf("republish: %v", err)
	}

	// Scrapping the newest moves the head back, and a client that already took
	// it refuses to follow.
	if err := p.Scrap(ctx, commit('b'), ReasonBroken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme/dev/builds", commit('b'))); !os.IsNotExist(err) {
		t.Error("scrapped build's files are still there")
	}
	if _, err := c.Latest(ctx, 2); !errors.Is(err, ErrRollback) {
		t.Errorf("after scrap, client at 2: %v", err)
	}
	j, err := c.Journal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s, r := j.Lookup(commit('b')); s != "scrap" || r != ReasonBroken {
		t.Errorf("journal says %q %q", s, r)
	}
}

func TestClientBelievesOnlyTheSignature(t *testing.T) {
	ctx := context.Background()
	_, c, root := publishAndServe(t)
	manifest := filepath.Join(root, "acme/dev/builds", commit('b'), "manifest")
	orig, _ := os.ReadFile(manifest)

	// Tampered manifest.
	os.WriteFile(manifest, []byte(strings.Replace(string(orig), "size=20", "size=21", 1)), 0o644)
	if _, err := c.Latest(ctx, 0); err == nil {
		t.Error("a tampered manifest was accepted")
	}
	os.WriteFile(manifest, orig, 0o644)

	// A validly signed manifest served for the wrong commit.
	aDir, bDir := filepath.Join(root, "acme/dev/builds", commit('a')), filepath.Join(root, "acme/dev/builds", commit('b'))
	for _, f := range []string{"manifest", "manifest.sig"} {
		data, _ := os.ReadFile(filepath.Join(aDir, f))
		os.WriteFile(filepath.Join(bDir, f), data, 0o644)
	}
	if _, err := c.Latest(ctx, 0); err == nil || !strings.Contains(err.Error(), "was served") {
		t.Errorf("a's manifest served as b's: %v", err)
	}

	// The same files, read as another channel.
	other := *c
	other.Channel = "stable"
	os.Rename(filepath.Join(root, "acme/dev"), filepath.Join(root, "acme/stable"))
	if _, err := other.Manifest(ctx, commit('a')); err == nil || !strings.Contains(err.Error(), "was served") {
		t.Errorf("dev's manifest served on stable: %v", err)
	}

	// A key that is not pinned.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	other.Keys = []ed25519.PublicKey{pub}
	if _, err := other.Manifest(ctx, commit('a')); err == nil {
		t.Error("an unpinned key was accepted")
	}
}

// Deleting a build from the store does not stop a cache serving it. Whatever
// sits in front is told, for a scrap and for an expiry alike, after the files
// have gone.
func TestRemovedBuildsArePurgedFromTheCache(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	art := filepath.Join(t.TempDir(), "tool")
	os.WriteFile(art, []byte("x"), 0o644)
	root := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var purged []string
	p := &Publisher{Store: store.Dir(root), Signer: sshsig.Key(priv), Project: "acme", Channel: "dev",
		Retention: time.Hour, Now: func() time.Time { return now },
		Purge: func(_ context.Context, prefix string) error {
			if _, err := os.Stat(filepath.Join(root, prefix)); !os.IsNotExist(err) && !strings.HasSuffix(prefix, "/latest/") {
				t.Errorf("purged %s while its files were still there", prefix)
			}
			purged = append(purged, prefix)
			return nil
		}}
	pub := func(c byte) {
		t.Helper()
		if _, err := p.Publish(ctx, Build{Commit: commit(c), Version: "v", Time: now}, []Upload{{Name: "tool", OS: "linux", Arch: "amd64", Local: art}}); err != nil {
			t.Fatal(err)
		}
	}
	pub('a')
	now = now.Add(time.Minute)
	pub('b')
	now = now.Add(24 * time.Hour)
	pub('c') // a was superseded a day ago: it expires
	if err := p.Scrap(ctx, commit('c'), ReasonSecurity); err != nil {
		t.Fatal(err)
	}
	// latest/ is purged whenever the newest build changes — and *before* an
	// expired build's files go, since save comes first.
	latest := "acme/dev/latest/"
	want := []string{latest, latest, latest, "acme/dev/builds/" + commit('a') + "/", latest, "acme/dev/builds/" + commit('c') + "/"}
	if strings.Join(purged, " ") != strings.Join(want, " ") {
		t.Errorf("purged %v, want %v", purged, want)
	}
}

func TestPublishRefusesArtifactsThatWouldShareAName(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	a, b := filepath.Join(t.TempDir(), "tool"), filepath.Join(t.TempDir(), "tool")
	os.WriteFile(a, []byte("linux"), 0o644)
	os.WriteFile(b, []byte("darwin"), 0o644)
	p := &Publisher{Store: store.Dir(t.TempDir()), Signer: sshsig.Key(priv), Project: "acme", Channel: "dev"}
	_, err := p.Publish(context.Background(), Build{Commit: commit('a'), Version: "v", Time: time.Unix(0, 0)},
		[]Upload{{Name: "tool", OS: "linux", Arch: "amd64", Local: a}, {Name: "tool", OS: "darwin", Arch: "arm64", Local: b}})
	if err == nil || !strings.Contains(err.Error(), "own name") {
		t.Fatalf("two files called tool were accepted: %v", err)
	}
	// Refused before anything was recorded: the commit can still be published.
	if _, err := p.Publish(context.Background(), Build{Commit: commit('a'), Version: "v", Time: time.Unix(0, 0)},
		[]Upload{{Name: "tool", OS: "linux", Arch: "amd64", Local: a}}); err != nil {
		t.Fatalf("the refusal left something behind: %v", err)
	}
}

func TestRetentionNeverExpiresTheNewest(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	art := filepath.Join(t.TempDir(), "acme")
	os.WriteFile(art, []byte("x"), 0o644)
	root := t.TempDir()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &Publisher{Store: store.Dir(root), Signer: sshsig.Key(priv), Project: "acme", Channel: "dev",
		Retention: 30 * 24 * time.Hour, Now: func() time.Time { return now }}
	pub := func(c byte) {
		t.Helper()
		if _, err := p.Publish(ctx, Build{Commit: commit(c), Version: "v", Time: now, MinEnboot: 1}, []Upload{{Name: "acme", OS: "linux", Arch: "amd64", Local: art}}); err != nil {
			t.Fatal(err)
		}
	}
	pub('a')
	now = now.Add(24 * time.Hour)
	pub('b')
	// A year of silence, then one push: a has been superseded for a year and
	// goes; b was the newest the whole time, is superseded only now, and stays.
	now = now.Add(365 * 24 * time.Hour)
	pub('c')

	data, _, _ := p.Store.Get(ctx, "acme/dev/journal")
	j, _ := ParseJournal(data)
	var live []string
	for _, b := range j.Live() {
		live = append(live, b.Commit[:1])
	}
	if strings.Join(live, "") != "bc" {
		t.Errorf("live = %v, want b and c\n%s", live, data)
	}
	if _, err := os.Stat(filepath.Join(root, "acme/dev/builds", commit('a'))); !os.IsNotExist(err) {
		t.Error("expired build's files are still there")
	}
}

func TestLatestMirrorsTheNewestBuild(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	root := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &Publisher{Store: store.Dir(root), Signer: sshsig.Key(priv), Project: "acme", Channel: "dev",
		Now: func() time.Time { return now }}
	pub := func(c byte, name string) {
		t.Helper()
		art := filepath.Join(dir, name)
		os.WriteFile(art, []byte("build "+string(c)), 0o644)
		if _, err := p.Publish(ctx, Build{Commit: commit(c), Version: "v", Time: now}, []Upload{{Name: "tool", OS: "linux", Arch: "amd64", Local: art}}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(root, "acme/dev/latest", name))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(data)
	}
	latestIs := func(c byte, name string) {
		t.Helper()
		m, err := ParseManifest([]byte(read("manifest")))
		if err != nil || m.Build.Commit != commit(c) {
			t.Fatalf("latest/manifest: %v %v, want %s", m, err, commit(c))
		}
		if got := read(name); got != "build "+string(c) {
			t.Fatalf("latest/%s = %q", name, got)
		}
		sig, err := os.ReadFile(filepath.Join(root, "acme/dev/builds", commit(c), "manifest.sig"))
		if err != nil || read("manifest.sig") != string(sig) {
			t.Fatalf("latest/manifest.sig is not the build's")
		}
	}

	pub('a', "tool_linux_amd64")
	latestIs('a', "tool_linux_amd64")
	pub('b', "tool_linux_amd64")
	latestIs('b', "tool_linux_amd64")

	// A build whose files are named differently leaves nothing stale behind.
	pub('c', "tool-linux-amd64")
	latestIs('c', "tool-linux-amd64")
	if got := read("tool_linux_amd64"); !strings.HasPrefix(got, "<") {
		t.Errorf("stale latest/tool_linux_amd64 = %q", got)
	}

	// Scrapping the newest moves latest/ back with the head.
	if err := p.Scrap(ctx, commit('c'), ReasonBroken); err != nil {
		t.Fatal(err)
	}
	latestIs('b', "tool_linux_amd64")
	if got := read("tool-linux-amd64"); !strings.HasPrefix(got, "<") {
		t.Errorf("scrapped build's file still in latest/: %q", got)
	}

	// A channel with nothing live has no latest/.
	if err := p.Scrap(ctx, commit('b'), ReasonBroken); err != nil {
		t.Fatal(err)
	}
	if err := p.Scrap(ctx, commit('a'), ReasonBroken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme/dev/latest")); !os.IsNotExist(err) {
		t.Errorf("latest/ still there with nothing live: %v", err)
	}
}
