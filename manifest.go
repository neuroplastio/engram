package engram

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Artifact is one file of a build.
type Artifact struct {
	Name, OS, Arch string
	Size           int64
	SHA256         string
	Path           string // relative to the manifest
}

// Manifest is what one build consists of. It is the only signed file, so it
// carries everything a client must be able to trust: which project and channel
// it belongs to, where it sits in the channel's sequence, and the hash of every
// artifact.
type Manifest struct {
	Project, Channel string
	Build            Build
	Artifacts        []Artifact
}

// Encode writes the manifest.
func (m *Manifest) Encode() ([]byte, error) {
	f := &File{Kind: KindManifest}
	f.Records = append(f.Records, buildRecord("build", m.Build, []Field{
		{"project", m.Project},
		{"channel", m.Channel},
	}))
	for _, a := range m.Artifacts {
		if err := checkPath(a.Path); err != nil {
			return nil, err
		}
		f.Records = append(f.Records, Record{Type: "artifact", Fields: []Field{
			{"name", a.Name},
			{"os", a.OS},
			{"arch", a.Arch},
			{"size", strconv.FormatInt(a.Size, 10)},
			{"sha256", a.SHA256},
			{"path", a.Path},
		}})
	}
	return f.Encode()
}

// ParseManifest reads a manifest. It does not verify anything: see Verify.
func ParseManifest(data []byte) (*Manifest, error) {
	f, err := Parse(bytes.NewReader(data), KindManifest)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	for _, r := range f.Records {
		switch r.Type {
		case "build":
			if m.Project, err = r.need("project"); err != nil {
				return nil, err
			}
			if m.Channel, err = r.need("channel"); err != nil {
				return nil, err
			}
			if m.Build, err = parseBuild(r); err != nil {
				return nil, err
			}
		case "artifact":
			a, err := parseArtifact(r)
			if err != nil {
				return nil, err
			}
			m.Artifacts = append(m.Artifacts, a)
		}
	}
	if m.Project == "" {
		return nil, errors.New("engram: manifest has no build record")
	}
	return m, nil
}

func parseArtifact(r Record) (Artifact, error) {
	var a Artifact
	var err error
	for key, dst := range map[string]*string{"name": &a.Name, "os": &a.OS, "arch": &a.Arch, "sha256": &a.SHA256, "path": &a.Path} {
		if *dst, err = r.need(key); err != nil {
			return a, err
		}
	}
	size, err := r.need("size")
	if err != nil {
		return a, err
	}
	if a.Size, err = strconv.ParseInt(size, 10, 64); err != nil || a.Size < 0 {
		return a, fmt.Errorf("artifact record: size=%q is not a number", size)
	}
	if len(a.SHA256) != 64 {
		return a, fmt.Errorf("artifact record: sha256 is not 64 hex characters")
	}
	return a, checkPath(a.Path)
}

// checkPath keeps an artifact inside its build's directory. A manifest is
// signed, but a client should not have to trust its publisher with the
// filesystem.
func checkPath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("engram: artifact path %q must be a clean relative path", p)
	}
	return nil
}

// Find returns the artifact for a name and platform.
func (m *Manifest) Find(name, os, arch string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.Name == name && a.OS == os && a.Arch == arch {
			return a, true
		}
	}
	return Artifact{}, false
}

// Check verifies downloaded bytes against an artifact's size and hash.
func (a Artifact) Check(data []byte) error {
	if int64(len(data)) != a.Size {
		return fmt.Errorf("engram: %s: size %d, manifest says %d", a.Path, len(data), a.Size)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != a.SHA256 {
		return fmt.Errorf("engram: %s: sha256 does not match the manifest", a.Path)
	}
	return nil
}
