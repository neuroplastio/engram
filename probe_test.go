package engram

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neuroplastio/engram/sshsig"
	"github.com/neuroplastio/engram/store"
)

func TestProbeReadsLittleAndCatchesWhatIsWrong(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := sshsig.Key(priv)
	root, built := t.TempDir(), t.TempDir()

	body := make([]byte, 5<<20)
	rand.Read(body)
	local := filepath.Join(built, "acme_linux_amd64")
	os.WriteFile(local, body, 0o644)
	now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	p := &Publisher{Store: store.Dir(root), Signer: key, Project: "acme", Channel: "dev", Now: func() time.Time { return now }}
	if _, err := p.Publish(ctx, Build{Commit: commit('a'), Version: "v", Time: now, MinEnboot: 1},
		[]Upload{{Name: "acme", OS: "linux", Arch: "amd64", Local: local}}); err != nil {
		t.Fatal(err)
	}

	// Counts the body bytes the server sends.
	var sent atomic.Int64
	files := http.FileServer(http.Dir(root))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files.ServeHTTP(&countWriter{ResponseWriter: w, n: &sent}, r)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Project: "acme", Channel: "dev", Keys: []ed25519.PublicKey{key.Public()}}
	m, err := c.Manifest(ctx, commit('a'))
	if err != nil {
		t.Fatal(err)
	}
	a := m.Artifacts[0]
	served := filepath.Join(root, "acme/dev/builds", commit('a'), a.Path)

	sent.Store(0)
	if err := c.Probe(ctx, m, a, body); err != nil {
		t.Fatalf("a good build: %v", err)
	}
	if got := sent.Load(); got != 2*ProbeBytes {
		t.Errorf("probe read %d bytes of a %d-byte artifact, want %d", got, len(body), 2*ProbeBytes)
	}

	flip := func(b []byte, at int) []byte {
		b = bytes.Clone(b)
		b[at] ^= 1
		return b
	}
	for name, tc := range map[string]struct {
		local []byte // what the build has
		cdn   []byte // what the server holds; nil leaves it be, empty removes it
		want  string
	}{
		"tampered local file":       {local: flip(body, 100), want: "sha256"},
		"short local file":          {local: body[:len(body)-1], want: "size"},
		"cdn serves another head":   {local: body, cdn: flip(body, 100), want: "bytes 0-"},
		"cdn serves another tail":   {local: body, cdn: flip(body, len(body)-5), want: "bytes "},
		"cdn serves another length": {local: body, cdn: append(bytes.Clone(body), 0), want: "length"},
		"cdn does not have it":      {local: body, cdn: []byte{}, want: "no such build"},
	} {
		t.Run(name, func(t *testing.T) {
			defer os.WriteFile(served, body, 0o644)
			switch {
			case tc.cdn != nil && len(tc.cdn) == 0:
				os.Remove(served)
			case tc.cdn != nil:
				os.WriteFile(served, tc.cdn, 0o644)
			}
			err := c.Probe(ctx, m, a, tc.local)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

type countWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (w *countWriter) Write(b []byte) (int, error) {
	w.n.Add(int64(len(b)))
	return w.ResponseWriter.Write(b)
}
