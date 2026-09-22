// Package store is where a channel's files live, as far as the publisher is
// concerned: a flat namespace of keys. Dir is a directory, for tests and dry
// runs; the S3 implementation is in awsx.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound means there is no such key.
var ErrNotFound = errors.New("store: not found")

// ErrConflict means a conditional Put lost: the key changed since it was read.
var ErrConflict = errors.New("store: changed since it was read")

// PutOptions says how a key is written.
type PutOptions struct {
	// CacheControl is served with the object. Lifetimes are the publisher's to
	// declare: the CDN in front of the bucket has no per-path rules.
	CacheControl string
	ContentType  string

	// IfMatch makes the write conditional on the tag returned by the Get that
	// preceded it; IfAbsent on the key not existing. They are what makes a
	// read-modify-write of the journal safe against a concurrent publisher.
	IfMatch  string
	IfAbsent bool
}

// Store is the publisher's view of a bucket.
type Store interface {
	// Get returns the content and a tag identifying this version of it.
	Get(ctx context.Context, key string) (data []byte, tag string, err error)
	Put(ctx context.Context, key string, data []byte, opts PutOptions) error
	// Copy makes dst a copy of src, served with opts — on the server where
	// the store has one, so that a mirror of a build costs no download.
	Copy(ctx context.Context, src, dst string, opts PutOptions) error
	// Delete removes one key. A key that is not there is not an error.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every key under prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// Dir is a Store in a directory. Its tags are content hashes, and its
// conditional writes are not atomic: good for one process, not for two.
type Dir string

func (d Dir) path(key string) string { return filepath.Join(string(d), filepath.FromSlash(key)) }

func (d Dir) Get(_ context.Context, key string) ([]byte, string, error) {
	data, err := os.ReadFile(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	return data, tag(data), err
}

func (d Dir) Put(ctx context.Context, key string, data []byte, opts PutOptions) error {
	if opts.IfMatch != "" || opts.IfAbsent {
		_, current, err := d.Get(ctx, key)
		switch {
		case errors.Is(err, ErrNotFound) && opts.IfMatch != "":
			return ErrConflict
		case err == nil && (opts.IfAbsent || current != opts.IfMatch):
			return ErrConflict
		case err != nil && !errors.Is(err, ErrNotFound):
			return err
		}
	}
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (d Dir) Copy(ctx context.Context, src, dst string, opts PutOptions) error {
	data, _, err := d.Get(ctx, src)
	if err != nil {
		return err
	}
	return d.Put(ctx, dst, data, opts)
}

func (d Dir) Delete(_ context.Context, key string) error {
	err := os.Remove(d.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (d Dir) DeletePrefix(_ context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") {
		return errors.New("store: a prefix to delete must end in /")
	}
	return os.RemoveAll(d.path(prefix))
}

func tag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
