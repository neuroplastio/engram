// Package sshsig makes and checks OpenSSH signatures (PROTOCOL.sshsig) with
// Ed25519 keys — the format `ssh-keygen -Y sign` writes and `ssh-keygen -Y
// verify` reads. It uses only the standard library: the whole verifier is a
// few length-prefixed fields around one Ed25519 check, which is the reason
// this format was chosen.
package sshsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

const (
	magic   = "SSHSIG"
	keyType = "ssh-ed25519"
	hashAlg = "sha512"
	pemType = "SSH SIGNATURE"
)

// Signer signs with an Ed25519 key it may not be able to hand over — one held
// in a KMS, say. Sign receives the exact bytes to sign and must return the raw
// 64-byte signature (pure Ed25519, no prehash).
type Signer interface {
	Public() ed25519.PublicKey
	Sign(message []byte) ([]byte, error)
}

// Key is a Signer for a key held in memory.
type Key ed25519.PrivateKey

func (k Key) Public() ed25519.PublicKey { return ed25519.PrivateKey(k).Public().(ed25519.PublicKey) }

func (k Key) Sign(message []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(k), message), nil
}

// Sign returns an armored signature of message under namespace.
func Sign(s Signer, namespace string, message []byte) ([]byte, error) {
	if namespace == "" {
		return nil, errors.New("sshsig: empty namespace")
	}
	raw, err := s.Sign(signedData(namespace, message))
	if err != nil {
		return nil, fmt.Errorf("sshsig: %w", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("sshsig: signer returned %d bytes, want %d", len(raw), ed25519.SignatureSize)
	}

	var b bytes.Buffer
	b.WriteString(magic)
	binary.Write(&b, binary.BigEndian, uint32(1))
	putString(&b, publicBlob(s.Public()))
	putString(&b, []byte(namespace))
	putString(&b, nil) // reserved
	putString(&b, []byte(hashAlg))
	var sig bytes.Buffer
	putString(&sig, []byte(keyType))
	putString(&sig, raw)
	putString(&b, sig.Bytes())

	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: b.Bytes()}), nil
}

// Verify checks that armored is a signature of message under namespace by one
// of the allowed keys, and returns the key that made it.
func Verify(allowed []ed25519.PublicKey, namespace string, message, armored []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(armored)
	if block == nil || block.Type != pemType {
		return nil, errors.New("sshsig: not an SSH signature")
	}
	r := bytes.NewReader(block.Bytes)

	head := make([]byte, len(magic))
	if _, err := r.Read(head); err != nil || string(head) != magic {
		return nil, errors.New("sshsig: bad preamble")
	}
	var version uint32
	if err := binary.Read(r, binary.BigEndian, &version); err != nil || version != 1 {
		return nil, errors.New("sshsig: unsupported signature version")
	}
	pubBlob, err := getString(r)
	if err != nil {
		return nil, err
	}
	ns, err := getString(r)
	if err != nil {
		return nil, err
	}
	if _, err := getString(r); err != nil { // reserved
		return nil, err
	}
	alg, err := getString(r)
	if err != nil {
		return nil, err
	}
	sigBlob, err := getString(r)
	if err != nil {
		return nil, err
	}

	// The namespace is inside the signed data, so this comparison is only a
	// better error message; a wrong namespace fails the signature regardless.
	if string(ns) != namespace {
		return nil, fmt.Errorf("sshsig: signature is for namespace %q, not %q", ns, namespace)
	}
	if string(alg) != hashAlg {
		return nil, fmt.Errorf("sshsig: unsupported hash %q", alg)
	}
	pub, err := parsePublicBlob(pubBlob)
	if err != nil {
		return nil, err
	}
	sr := bytes.NewReader(sigBlob)
	if t, err := getString(sr); err != nil || string(t) != keyType {
		return nil, errors.New("sshsig: not an ed25519 signature")
	}
	raw, err := getString(sr)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("sshsig: malformed signature")
	}

	// The key in the signature is only a hint. What matters is that it is one
	// of ours.
	for _, k := range allowed {
		if k.Equal(pub) {
			if !ed25519.Verify(k, signedData(namespace, message), raw) {
				return nil, errors.New("sshsig: signature does not verify")
			}
			return k, nil
		}
	}
	return nil, errors.New("sshsig: signed by a key that is not allowed")
}

func signedData(namespace string, message []byte) []byte {
	sum := sha512.Sum512(message)
	var b bytes.Buffer
	b.WriteString(magic)
	putString(&b, []byte(namespace))
	putString(&b, nil) // reserved
	putString(&b, []byte(hashAlg))
	putString(&b, sum[:])
	return b.Bytes()
}

// AuthorizedKey renders a public key as an OpenSSH public key line.
func AuthorizedKey(pub ed25519.PublicKey) string {
	return keyType + " " + base64.StdEncoding.EncodeToString(publicBlob(pub))
}

// AllowedSigners renders an allowed_signers line for `ssh-keygen -Y verify`.
func AllowedSigners(identity, namespace string, pub ed25519.PublicKey) string {
	return fmt.Sprintf("%s namespaces=%q %s\n", identity, namespace, AuthorizedKey(pub))
}

// ParseAllowedSigners reads the ed25519 keys out of an allowed_signers file (or
// a plain list of OpenSSH public keys). Principals and options are ignored: a
// client pins keys, not names.
func ParseAllowedSigners(data []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != keyType || i+1 >= len(fields) {
				continue
			}
			blob, err := base64.StdEncoding.DecodeString(fields[i+1])
			if err != nil {
				return nil, fmt.Errorf("sshsig: bad key: %w", err)
			}
			k, err := parsePublicBlob(blob)
			if err != nil {
				return nil, err
			}
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("sshsig: no ed25519 keys found")
	}
	return keys, nil
}

func publicBlob(pub ed25519.PublicKey) []byte {
	var b bytes.Buffer
	putString(&b, []byte(keyType))
	putString(&b, pub)
	return b.Bytes()
}

func parsePublicBlob(blob []byte) (ed25519.PublicKey, error) {
	r := bytes.NewReader(blob)
	if t, err := getString(r); err != nil || string(t) != keyType {
		return nil, errors.New("sshsig: not an ed25519 key")
	}
	k, err := getString(r)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return nil, errors.New("sshsig: malformed ed25519 key")
	}
	return ed25519.PublicKey(k), nil
}

func putString(b *bytes.Buffer, s []byte) {
	binary.Write(b, binary.BigEndian, uint32(len(s)))
	b.Write(s)
}

func getString(r *bytes.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, errors.New("sshsig: truncated")
	}
	if int64(n) > int64(r.Len()) {
		return nil, errors.New("sshsig: truncated")
	}
	s := make([]byte, n)
	if _, err := r.Read(s); err != nil && n > 0 {
		return nil, errors.New("sshsig: truncated")
	}
	return s, nil
}
