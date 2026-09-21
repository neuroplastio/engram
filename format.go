// Package engram reads and writes release channels: an append-only journal of
// what a project published and took back, a small head for polling, and one
// signed manifest per build. SPEC.md is the contract; this package is its
// reference implementation, and it depends on nothing outside the standard
// library so that a client can verify a release without importing a cloud SDK.
package engram

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Magic is the first word of every engram file.
const Magic = "engram"

// Version is the major version this package reads and writes.
const Version = "1"

// The three kinds of file.
const (
	KindJournal  = "journal"
	KindHead     = "head"
	KindManifest = "manifest"
)

// MaxLine bounds a line. A reader meant to live in a canned binary needs a
// fixed buffer, so the limit is part of the format rather than an accident of
// this implementation.
const MaxLine = 4096

// Field is one key=value pair. Order is preserved so that a record can be
// re-encoded byte for byte.
type Field struct {
	Key, Value string
}

// Record is one line: a type word, then fields.
type Record struct {
	Type   string
	Fields []Field
}

// Get returns the first value for key.
func (r Record) Get(key string) (string, bool) {
	for _, f := range r.Fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return "", false
}

// need returns the value for key, or an error naming the record and the key.
func (r Record) need(key string) (string, error) {
	v, ok := r.Get(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%s record: missing %s", r.Type, key)
	}
	return v, nil
}

// String encodes the record as a line, without the newline.
func (r Record) String() string {
	var b strings.Builder
	b.WriteString(r.Type)
	for _, f := range r.Fields {
		b.WriteByte(' ')
		b.WriteString(f.Key)
		b.WriteByte('=')
		b.WriteString(f.Value)
	}
	return b.String()
}

// File is a parsed engram file: its kind, and every record after the first
// line, unknown ones included.
type File struct {
	Kind    string
	Records []Record
}

// ErrVersion means the file is a major version this package does not read.
var ErrVersion = errors.New("engram: unsupported version")

// Parse reads a file of the given kind. It refuses a file that says it is a
// different kind: the path a file was fetched from is not evidence of what it
// is.
func Parse(r io.Reader, kind string) (*File, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, MaxLine), MaxLine)

	f := &File{}
	first := true
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || line[0] == '#' {
			continue
		}
		rec, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("engram: line %d: %w", n, err)
		}
		if first {
			first = false
			if rec.Type != Magic {
				return nil, fmt.Errorf("engram: line %d: not an engram file", n)
			}
			if v, _ := rec.Get("version"); v != Version {
				return nil, fmt.Errorf("%w %q", ErrVersion, v)
			}
			f.Kind, _ = rec.Get("kind")
			if f.Kind != kind {
				return nil, fmt.Errorf("engram: expected a %s, got a %q", kind, f.Kind)
			}
			continue
		}
		f.Records = append(f.Records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("engram: %w", err)
	}
	if first {
		return nil, errors.New("engram: empty file")
	}
	return f, nil
}

// parseLine splits on runs of spaces, then each token on its first '='. That
// is the whole grammar, and it has to stay that way: see SPEC.md, "A reader
// needs no library".
func parseLine(line string) (Record, error) {
	if strings.ContainsAny(line, "\"\t\r") {
		return Record{}, errors.New("quotes, tabs and carriage returns are not allowed")
	}
	tokens := strings.Fields(line)
	rec := Record{Type: tokens[0]}
	if strings.Contains(rec.Type, "=") {
		return Record{}, errors.New("a line starts with a record type, not a field")
	}
	for _, t := range tokens[1:] {
		k, v, ok := strings.Cut(t, "=")
		if !ok || k == "" {
			return Record{}, fmt.Errorf("%q is not key=value", t)
		}
		rec.Fields = append(rec.Fields, Field{k, v})
	}
	return rec, nil
}

// Encode writes the file. It fails on a value the grammar cannot carry rather
// than inventing an escape for it.
func (f *File) Encode() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s version=%s kind=%s\n", Magic, Version, f.Kind)
	for _, r := range f.Records {
		if err := checkToken(r.Type, false); err != nil {
			return nil, fmt.Errorf("engram: record type: %w", err)
		}
		for _, fl := range r.Fields {
			if err := checkToken(fl.Key, false); err != nil {
				return nil, fmt.Errorf("engram: %s: key: %w", r.Type, err)
			}
			if err := checkToken(fl.Value, true); err != nil {
				return nil, fmt.Errorf("engram: %s %s: %w", r.Type, fl.Key, err)
			}
		}
		line := r.String()
		if len(line) >= MaxLine {
			return nil, fmt.Errorf("engram: %s record is longer than %d bytes", r.Type, MaxLine)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// checkToken enforces the no-quotes subset. Values may contain '=' (the split
// is on the first one); keys and types may not.
func checkToken(s string, value bool) error {
	if s == "" {
		return errors.New("empty")
	}
	for _, c := range s {
		switch {
		case c <= ' ' || c == 0x7f || c == '"':
			return fmt.Errorf("%q contains a character the format cannot carry", s)
		case c == '=' && !value:
			return fmt.Errorf("%q contains '='", s)
		}
	}
	return nil
}
