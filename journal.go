package engram

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Build identifies one published build. Commit is the identity; Version is for
// people and is never compared or sorted.
type Build struct {
	Seq       int
	Commit    string
	Version   string
	Time      time.Time
	MinEnboot int
}

// ErrPublished means the commit is already in the journal. A release workflow
// that is re-run after a partial failure should treat it as success.
var ErrPublished = errors.New("engram: commit already published")

// Scrap reasons. An unknown reason reads as absent.
const (
	ReasonBroken   = "broken"
	ReasonSecurity = "security"
	ReasonMistake  = "mistake"
)

// Journal is a channel's history. Records are kept exactly as read, unknown
// ones included, so that appending an event never rewrites what came before.
type Journal struct {
	Project, Channel string

	records []Record
	live    map[string]Build // commit -> build, for commits still live
	seen    map[string]bool  // every commit ever published
	lastSeq int
}

// NewJournal starts an empty channel.
func NewJournal(project, channel string) *Journal {
	j := &Journal{Project: project, Channel: channel, live: map[string]Build{}, seen: map[string]bool{}}
	j.records = []Record{{Type: "channel", Fields: []Field{{"project", project}, {"name", channel}}}}
	return j
}

// ParseJournal reads a journal and folds its events.
func ParseJournal(data []byte) (*Journal, error) {
	f, err := Parse(bytes.NewReader(data), KindJournal)
	if err != nil {
		return nil, err
	}
	j := &Journal{records: f.Records, live: map[string]Build{}, seen: map[string]bool{}}
	for _, r := range f.Records {
		switch r.Type {
		case "channel":
			if j.Project, err = r.need("project"); err != nil {
				return nil, err
			}
			if j.Channel, err = r.need("name"); err != nil {
				return nil, err
			}
		case "publish":
			b, err := parseBuild(r)
			if err != nil {
				return nil, err
			}
			if err := j.advance(b.Seq); err != nil {
				return nil, err
			}
			if j.seen[b.Commit] {
				return nil, fmt.Errorf("engram: commit %s published twice", b.Commit)
			}
			j.seen[b.Commit] = true
			j.live[b.Commit] = b
		case "scrap", "expire":
			seq, commit, err := parseRemoval(r)
			if err != nil {
				return nil, err
			}
			if err := j.advance(seq); err != nil {
				return nil, err
			}
			delete(j.live, commit)
		}
	}
	if j.Project == "" {
		return nil, errors.New("engram: journal has no channel record")
	}
	return j, nil
}

func (j *Journal) advance(seq int) error {
	if seq != j.lastSeq+1 {
		return fmt.Errorf("engram: sequence %d follows %d", seq, j.lastSeq)
	}
	j.lastSeq = seq
	return nil
}

// LastSeq is the sequence of the newest event; 0 for an empty journal.
func (j *Journal) LastSeq() int { return j.lastSeq }

// Live returns the builds still live, oldest publish first.
func (j *Journal) Live() []Build {
	out := make([]Build, 0, len(j.live))
	for _, r := range j.records {
		if r.Type != "publish" {
			continue
		}
		c, _ := r.Get("commit")
		if b, ok := j.live[c]; ok {
			out = append(out, b)
		}
	}
	return out
}

// Newest returns the newest live build.
func (j *Journal) Newest() (Build, bool) {
	l := j.Live()
	if len(l) == 0 {
		return Build{}, false
	}
	return l[len(l)-1], true
}

// Lookup says what the journal knows about a commit: live, or removed and why.
// status is "live", "scrap", "expire", or "" for a commit never published.
func (j *Journal) Lookup(commit string) (status, reason string) {
	if _, ok := j.live[commit]; ok {
		return "live", ""
	}
	for i := len(j.records) - 1; i >= 0; i-- {
		r := j.records[i]
		if r.Type != "scrap" && r.Type != "expire" {
			continue
		}
		if c, _ := r.Get("commit"); c == commit {
			reason, _ = r.Get("reason")
			return r.Type, reason
		}
	}
	return "", ""
}

// Publish appends a publish event and returns the build with its sequence. A
// commit is published at most once: a scrapped or expired commit never returns.
func (j *Journal) Publish(b Build) (Build, error) {
	if len(b.Commit) != 40 {
		return Build{}, fmt.Errorf("engram: commit %q is not a full 40-character hash", b.Commit)
	}
	if j.seen[b.Commit] {
		return Build{}, fmt.Errorf("%w: %s on %s/%s", ErrPublished, b.Commit, j.Project, j.Channel)
	}
	j.lastSeq++
	b.Seq = j.lastSeq
	j.records = append(j.records, buildRecord("publish", b, nil))
	j.seen[b.Commit] = true
	j.live[b.Commit] = b
	return b, nil
}

// Scrap withdraws a live build. reason may be empty.
func (j *Journal) Scrap(commit, reason string, at time.Time) error {
	return j.remove("scrap", commit, reason, at)
}

// Expire records that a live build aged out.
func (j *Journal) Expire(commit string, at time.Time) error {
	return j.remove("expire", commit, "", at)
}

func (j *Journal) remove(kind, commit, reason string, at time.Time) error {
	if _, ok := j.live[commit]; !ok {
		return fmt.Errorf("engram: commit %s is not live on %s/%s", commit, j.Project, j.Channel)
	}
	j.lastSeq++
	r := Record{Type: kind, Fields: []Field{
		{"seq", strconv.Itoa(j.lastSeq)},
		{"commit", commit},
		{"time", at.UTC().Format(time.RFC3339)},
	}}
	if reason != "" {
		r.Fields = append(r.Fields, Field{"reason", reason})
	}
	j.records = append(j.records, r)
	delete(j.live, commit)
	return nil
}

// Encode writes the journal.
func (j *Journal) Encode() ([]byte, error) {
	return (&File{Kind: KindJournal, Records: j.records}).Encode()
}

// Head writes the head: the channel, how far the journal had got, and the
// newest live build exactly as the journal records it.
func (j *Journal) Head(updated time.Time) ([]byte, error) {
	f := &File{Kind: KindHead, Records: []Record{{Type: "channel", Fields: []Field{
		{"project", j.Project},
		{"name", j.Channel},
		{"seq", strconv.Itoa(j.lastSeq)},
		{"updated", updated.UTC().Format(time.RFC3339)},
	}}}}
	if b, ok := j.Newest(); ok {
		for _, r := range j.records {
			if c, _ := r.Get("commit"); r.Type == "publish" && c == b.Commit {
				f.Records = append(f.Records, r)
			}
		}
	}
	return f.Encode()
}

// Head is a parsed head file. Nothing in it is signed: it says where to look,
// and the manifest it points at says whether to believe it.
type Head struct {
	Project, Channel string
	Seq              int
	Build            *Build // nil when the channel has no live build
}

// ParseHead reads a head.
func ParseHead(data []byte) (*Head, error) {
	f, err := Parse(bytes.NewReader(data), KindHead)
	if err != nil {
		return nil, err
	}
	h := &Head{}
	for _, r := range f.Records {
		switch r.Type {
		case "channel":
			if h.Project, err = r.need("project"); err != nil {
				return nil, err
			}
			if h.Channel, err = r.need("name"); err != nil {
				return nil, err
			}
			if h.Seq, err = needInt(r, "seq"); err != nil {
				return nil, err
			}
		case "publish":
			b, err := parseBuild(r)
			if err != nil {
				return nil, err
			}
			h.Build = &b
		}
	}
	if h.Project == "" {
		return nil, errors.New("engram: head has no channel record")
	}
	return h, nil
}

func buildRecord(kind string, b Build, lead []Field) Record {
	return Record{Type: kind, Fields: append(lead,
		Field{"seq", strconv.Itoa(b.Seq)},
		Field{"commit", b.Commit},
		Field{"version", b.Version},
		Field{"time", b.Time.UTC().Format(time.RFC3339)},
		Field{"min_enboot", strconv.Itoa(b.MinEnboot)},
	)}
}

func parseBuild(r Record) (Build, error) {
	var b Build
	var err error
	if b.Seq, err = needInt(r, "seq"); err != nil {
		return b, err
	}
	if b.Commit, err = r.need("commit"); err != nil {
		return b, err
	}
	if len(b.Commit) != 40 {
		return b, fmt.Errorf("%s record: commit %q is not a full 40-character hash", r.Type, b.Commit)
	}
	if b.Version, err = r.need("version"); err != nil {
		return b, err
	}
	t, err := r.need("time")
	if err != nil {
		return b, err
	}
	if b.Time, err = time.Parse(time.RFC3339, t); err != nil {
		return b, fmt.Errorf("%s record: time: %w", r.Type, err)
	}
	if b.MinEnboot, err = needInt(r, "min_enboot"); err != nil {
		return b, err
	}
	return b, nil
}

func parseRemoval(r Record) (seq int, commit string, err error) {
	if seq, err = needInt(r, "seq"); err != nil {
		return
	}
	if commit, err = r.need("commit"); err != nil {
		return
	}
	_, err = r.need("time")
	return
}

func needInt(r Record, key string) (int, error) {
	v, err := r.need(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s record: %s=%q is not a number", r.Type, key, v)
	}
	return n, nil
}
