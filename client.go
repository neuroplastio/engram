package engram

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/neuroplastio/engram/sshsig"
)

// Size limits for the files a client reads. A head is three lines and a
// manifest a few dozen; anything larger is not what it claims to be.
const (
	maxHead     = 16 << 10
	maxManifest = 1 << 20
	maxJournal  = 64 << 20
)

// ErrNotLive means the channel has no such build: never published here,
// expired, or scrapped. The journal says which.
var ErrNotLive = errors.New("engram: no such build on this channel")

// ErrRollback means the channel pointed at a build older than one this client
// has already accepted.
var ErrRollback = errors.New("engram: refusing to go back to an older build")

// ErrUnauthorized means the channel is private and this client sent no token,
// or one the server refused.
var ErrUnauthorized = errors.New("engram: the channel refused access")

// Client reads one channel and believes only what a pinned key has signed.
type Client struct {
	Base             string // e.g. https://pkg.neuroplast.io
	Project, Channel string
	Keys             []ed25519.PublicKey
	HTTP             *http.Client // nil means http.DefaultClient
	// Token is a private channel's: sent as a bearer token on every request.
	// It decides what the server will serve, never what the client believes.
	Token string
}

func (c *Client) url(elem ...string) string {
	return strings.TrimRight(c.Base, "/") + "/" + c.Project + "/" + c.Channel + "/" + strings.Join(elem, "/")
}

// do sends one request; byteRange, if set, is a Range header value.
func (c *Client) do(ctx context.Context, method, url, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// statusErr is the error a response's status stands for, nil when it is the
// success status want.
func (c *Client) statusErr(resp *http.Response, method, url string, want int) error {
	switch {
	// A private bucket behind a CDN answers 403, not 404, for a missing key.
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return ErrNotLive
	// Which is why a private channel refuses with 401.
	case resp.StatusCode == http.StatusUnauthorized && c.Token == "":
		return fmt.Errorf("%w: it is private, and no token was given", ErrUnauthorized)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: the token was refused", ErrUnauthorized)
	case resp.StatusCode != want:
		return fmt.Errorf("engram: %s %s: %s", method, url, resp.Status)
	}
	return nil
}

func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, url, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := c.statusErr(resp, http.MethodGet, url, http.StatusOK); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("engram: %s is larger than %d bytes", url, limit)
	}
	return data, nil
}

// Head fetches the head. Nothing in it is signed; use it only to learn which
// manifest to ask for.
func (c *Client) Head(ctx context.Context) (*Head, error) {
	data, err := c.get(ctx, c.url("head"), maxHead)
	if err != nil {
		return nil, err
	}
	return ParseHead(data)
}

// Manifest fetches and verifies the manifest of a commit: signed by a pinned
// key, and for this project, this channel and this commit.
func (c *Client) Manifest(ctx context.Context, commit string) (*Manifest, error) {
	if len(commit) != 40 {
		return nil, fmt.Errorf("engram: commit %q is not a full 40-character hash", commit)
	}
	data, err := c.get(ctx, c.url("builds", commit, "manifest"), maxManifest)
	if err != nil {
		return nil, err
	}
	sig, err := c.get(ctx, c.url("builds", commit, "manifest.sig"), maxHead)
	if err != nil {
		return nil, err
	}
	if _, err := sshsig.Verify(c.Keys, Namespace, data, sig); err != nil {
		return nil, fmt.Errorf("engram: manifest of %s: %w", commit, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, err
	}
	if m.Project != c.Project || m.Channel != c.Channel || m.Build.Commit != commit {
		return nil, fmt.Errorf("engram: asked for %s/%s %s, was served a signed manifest for %s/%s %s",
			c.Project, c.Channel, commit, m.Project, m.Channel, m.Build.Commit)
	}
	return m, nil
}

// Latest resolves the head to a verified manifest. accepted is the highest
// sequence this client has taken from the channel before (0 for none); a
// channel pointing at anything older fails with ErrRollback. A nil manifest
// with a nil error means there is nothing newer.
func (c *Client) Latest(ctx context.Context, accepted int) (*Manifest, error) {
	h, err := c.Head(ctx)
	if err != nil {
		return nil, err
	}
	if h.Build == nil {
		return nil, nil
	}
	m, err := c.Manifest(ctx, h.Build.Commit)
	if err != nil {
		return nil, err
	}
	// The signed sequence, not the head's.
	switch {
	case m.Build.Seq < accepted:
		return nil, fmt.Errorf("%w: channel offers sequence %d, already at %d", ErrRollback, m.Build.Seq, accepted)
	case m.Build.Seq == accepted:
		return nil, nil
	}
	return m, nil
}

// Download fetches an artifact of a verified manifest and checks it.
func (c *Client) Download(ctx context.Context, m *Manifest, a Artifact) ([]byte, error) {
	data, err := c.get(ctx, c.url("builds", m.Build.Commit, a.Path), a.Size)
	if err != nil {
		return nil, err
	}
	return data, a.Check(data)
}

// ProbeBytes is how much of each end of an artifact Probe reads. A Go binary's
// build id sits near the start and its tables near the end, so two builds that
// differ rarely agree on both.
const ProbeBytes = 64 << 10

// Probe checks, without downloading the artifact, that the server serves the
// bytes of local, a file already checked against the manifest: HEAD for the
// status and Content-Length, then the first and last ProbeBytes by Range,
// compared with local's. It catches an artifact that is missing and a cache
// holding another build's object under the same path.
func (c *Client) Probe(ctx context.Context, m *Manifest, a Artifact, local []byte) error {
	if err := a.Check(local); err != nil {
		return err
	}
	url := c.url("builds", m.Build.Commit, a.Path)
	resp, err := c.do(ctx, http.MethodHead, url, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if err := c.statusErr(resp, http.MethodHead, url, http.StatusOK); err != nil {
		return fmt.Errorf("%s: %w", a.Path, err)
	}
	if resp.ContentLength != a.Size {
		return fmt.Errorf("engram: %s: served with length %d, manifest says %d", a.Path, resp.ContentLength, a.Size)
	}
	n := min(a.Size, ProbeBytes)
	if n == 0 {
		return nil
	}
	for _, at := range []int64{0, a.Size - n} {
		if err := c.probeRange(ctx, url, a, local, at, n); err != nil {
			return err
		}
		if n == a.Size {
			break
		}
	}
	return nil
}

func (c *Client) probeRange(ctx context.Context, url string, a Artifact, local []byte, at, n int64) error {
	resp, err := c.do(ctx, http.MethodGet, url, fmt.Sprintf("bytes=%d-%d", at, at+n-1))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A 200 is a server that ignored the range and is sending the whole file;
	// stopping here costs a little of it, never all of it.
	if err := c.statusErr(resp, http.MethodGet, url, http.StatusPartialContent); err != nil {
		return fmt.Errorf("%s: %w", a.Path, err)
	}
	got, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, local[at:at+n]) {
		return fmt.Errorf("engram: %s: bytes %d-%d are not the published build's", a.Path, at, at+n-1)
	}
	return nil
}

// Journal fetches the journal. It is unsigned: what it says about a build being
// scrapped or expired is advice to show the user, never grounds to act.
func (c *Client) Journal(ctx context.Context) (*Journal, error) {
	data, err := c.get(ctx, c.url("journal"), maxJournal)
	if err != nil {
		return nil, err
	}
	return ParseJournal(data)
}
