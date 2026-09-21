package engram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/neuroplastio/engram/sshsig"
	"github.com/neuroplastio/engram/store"
)

// Namespace is the OpenSSH signature namespace of every engram signature. It
// keeps a signature made for a manifest from meaning anything anywhere else.
const Namespace = "engram"

// Cache lifetimes. The files that move are short-lived; a build never changes
// once written, because a replaced release is a new commit and so a new path.
const (
	cacheMoving    = "public, max-age=60"
	cacheImmutable = "public, max-age=31536000, immutable"
)

const textPlain = "text/plain; charset=utf-8"

// Publisher writes to one channel.
type Publisher struct {
	Store            store.Store
	Signer           sshsig.Signer
	Project, Channel string

	// Retention is how long a build stays live after a newer one exists. Zero
	// keeps everything. The newest live build is never expired, so a channel
	// that goes quiet keeps a head that resolves.
	Retention time.Duration

	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Publisher) key(elem ...string) string {
	return path.Join(append([]string{p.Project, p.Channel}, elem...)...)
}

// Upload is an artifact to publish: what it is, and where it is on disk.
type Upload struct {
	Name, OS, Arch string
	Local          string
}

// Publish uploads a build, signs its manifest, and records it in the journal.
//
// The order matters. Everything a client could be pointed at exists before
// anything points at it, and the journal is written with a conditional put, so
// a concurrent publisher loses cleanly and retries from the top.
func (p *Publisher) Publish(ctx context.Context, b Build, files []Upload) (Build, error) {
	if len(files) == 0 {
		return Build{}, errors.New("engram: a build with no artifacts")
	}
	for attempt := 0; ; attempt++ {
		out, err := p.publishOnce(ctx, b, files)
		if errors.Is(err, store.ErrConflict) && attempt < 5 {
			continue
		}
		return out, err
	}
}

func (p *Publisher) publishOnce(ctx context.Context, b Build, files []Upload) (Build, error) {
	j, tag, err := p.load(ctx)
	if err != nil {
		return Build{}, err
	}
	b, err = j.Publish(b)
	if err != nil {
		return Build{}, err
	}

	m := &Manifest{Project: p.Project, Channel: p.Channel, Build: b}
	dir := p.key("builds", b.Commit)
	// A build's files share one directory, named by their base names. Four
	// platforms' worth of a binary called `tool` would overwrite each other and
	// leave a manifest with four hashes for one file.
	names := map[string]bool{}
	for _, f := range files {
		if name := path.Base(f.Local); names[name] || name == "manifest" || name == "manifest.sig" {
			return Build{}, fmt.Errorf("engram: two artifacts would be stored as %q — give each file its own name", name)
		} else {
			names[name] = true
		}
	}
	for _, f := range files {
		data, err := os.ReadFile(f.Local)
		if err != nil {
			return Build{}, err
		}
		sum := sha256.Sum256(data)
		a := Artifact{Name: f.Name, OS: f.OS, Arch: f.Arch, Size: int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]), Path: path.Base(f.Local)}
		if err := p.Store.Put(ctx, path.Join(dir, a.Path), data, store.PutOptions{CacheControl: cacheImmutable}); err != nil {
			return Build{}, fmt.Errorf("engram: upload %s: %w", a.Path, err)
		}
		m.Artifacts = append(m.Artifacts, a)
	}

	manifest, err := m.Encode()
	if err != nil {
		return Build{}, err
	}
	sig, err := sshsig.Sign(p.Signer, Namespace, manifest)
	if err != nil {
		return Build{}, err
	}
	for name, data := range map[string][]byte{"manifest": manifest, "manifest.sig": sig} {
		opts := store.PutOptions{CacheControl: cacheImmutable, ContentType: textPlain}
		if err := p.Store.Put(ctx, path.Join(dir, name), data, opts); err != nil {
			return Build{}, fmt.Errorf("engram: upload %s: %w", name, err)
		}
	}

	expired := p.expire(j)
	if err := p.save(ctx, j, tag); err != nil {
		return Build{}, err
	}
	// Only once the journal says they are gone. A failure here leaves files
	// nothing points at, which the next publish cannot see and a bucket
	// lifecycle rule should sweep; the reverse order would leave the journal
	// calling a missing build live.
	for _, commit := range expired {
		if err := p.Store.DeletePrefix(ctx, p.key("builds", commit)+"/"); err != nil {
			return b, fmt.Errorf("engram: published, but could not delete expired build %s: %w", commit, err)
		}
	}
	return b, nil
}

// expire records every live build that has been superseded for longer than the
// retention. "Superseded" is what keeps a quiet channel alive: a build's clock
// starts when the next one is published, not when it was.
func (p *Publisher) expire(j *Journal) []string {
	if p.Retention <= 0 {
		return nil
	}
	live := j.Live()
	var gone []string
	for i := 0; i+1 < len(live); i++ {
		if supersededAt := live[i+1].Time; p.now().Sub(supersededAt) > p.Retention {
			if j.Expire(live[i].Commit, p.now()) == nil {
				gone = append(gone, live[i].Commit)
			}
		}
	}
	return gone
}

// Scrap withdraws a build: the journal says so, then its files go.
func (p *Publisher) Scrap(ctx context.Context, commit, reason string) error {
	for attempt := 0; ; attempt++ {
		j, tag, err := p.load(ctx)
		if err != nil {
			return err
		}
		if err := j.Scrap(commit, reason, p.now()); err != nil {
			return err
		}
		err = p.save(ctx, j, tag)
		if errors.Is(err, store.ErrConflict) && attempt < 5 {
			continue
		}
		if err != nil {
			return err
		}
		return p.Store.DeletePrefix(ctx, p.key("builds", commit)+"/")
	}
}

func (p *Publisher) load(ctx context.Context) (*Journal, string, error) {
	data, tag, err := p.Store.Get(ctx, p.key("journal"))
	if errors.Is(err, store.ErrNotFound) {
		return NewJournal(p.Project, p.Channel), "", nil
	}
	if err != nil {
		return nil, "", err
	}
	j, err := ParseJournal(data)
	if err != nil {
		return nil, "", err
	}
	if j.Project != p.Project || j.Channel != p.Channel {
		return nil, "", fmt.Errorf("engram: journal at %s belongs to %s/%s", p.key("journal"), j.Project, j.Channel)
	}
	return j, tag, nil
}

// save writes the journal conditionally, then the head. A head that lags the
// journal is harmless — it points at an older build that is still live or at
// one the manifest check will refuse — so it needs no condition of its own.
func (p *Publisher) save(ctx context.Context, j *Journal, tag string) error {
	data, err := j.Encode()
	if err != nil {
		return err
	}
	opts := store.PutOptions{CacheControl: cacheMoving, ContentType: textPlain, IfMatch: tag, IfAbsent: tag == ""}
	if err := p.Store.Put(ctx, p.key("journal"), data, opts); err != nil {
		return err
	}
	head, err := j.Head(p.now())
	if err != nil {
		return err
	}
	return p.Store.Put(ctx, p.key("head"), head, store.PutOptions{CacheControl: cacheMoving, ContentType: textPlain})
}
