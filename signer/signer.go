// Package signer is where a publisher's Ed25519 key can live: in AWS KMS, where
// it cannot be exported and there is nothing to store, or in an OpenSSH key
// file, for anyone without a KMS. Both satisfy sshsig.Signer.
package signer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/neuroplastio/engram/sigv4"
)

// KMS signs with an Ed25519 key held in AWS KMS. It is two API calls —
// GetPublicKey once, Sign per manifest — made as plain signed HTTPS requests.
type KMS struct {
	KeyID       string // id, ARN or alias/name
	Region      string
	Credentials sigv4.Credentials
	HTTP        *http.Client

	public ed25519.PublicKey
}

// OpenKMS fetches the key's public half.
func OpenKMS(ctx context.Context, k KMS) (*KMS, error) {
	var out struct{ PublicKey string }
	if err := k.call(ctx, "GetPublicKey", map[string]string{"KeyId": k.KeyID}, &out); err != nil {
		return nil, err
	}
	der, err := base64.StdEncoding.DecodeString(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("kms: %s: %w", k.KeyID, err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("kms: %s is a %T, not an Ed25519 key", k.KeyID, parsed)
	}
	k.public = pub
	return &k, nil
}

func (k *KMS) Public() ed25519.PublicKey { return k.public }

func (k *KMS) Sign(message []byte) ([]byte, error) {
	var out struct{ Signature string }
	err := k.call(context.Background(), "Sign", map[string]string{
		"KeyId":            k.KeyID,
		"Message":          base64.StdEncoding.EncodeToString(message),
		"MessageType":      "RAW",
		"SigningAlgorithm": "ED25519_SHA_512",
	}, &out)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Signature)
}

func (k *KMS) call(ctx context.Context, op string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://kms."+k.Region+".amazonaws.com/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)
	if err := sigv4.Sign(req, k.Credentials, k.Region, "kms", time.Now()); err != nil {
		return err
	}
	hc := k.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		json.Unmarshal(data, &e)
		return fmt.Errorf("kms: %s: %s %s", op, e.Type, e.Message)
	}
	return json.Unmarshal(data, out)
}

// File is a key read from an unencrypted OpenSSH private key file
// (`ssh-keygen -t ed25519 -N ""`). A key on disk is a secret to look after;
// prefer a KMS where there is one.
type File ed25519.PrivateKey

func (f File) Public() ed25519.PublicKey { return ed25519.PrivateKey(f).Public().(ed25519.PublicKey) }

func (f File) Sign(message []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(f), message), nil
}

// OpenFile reads an openssh-key-v1 file holding one unencrypted Ed25519 key.
func OpenFile(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return nil, fmt.Errorf("%s: not an OpenSSH private key", path)
	}
	const magic = "openssh-key-v1\x00"
	if !bytes.HasPrefix(block.Bytes, []byte(magic)) {
		return nil, fmt.Errorf("%s: not an openssh-key-v1 file", path)
	}
	r := bytes.NewReader(block.Bytes[len(magic):])
	cipher, _ := str(r)
	str(r) // kdf name
	str(r) // kdf options
	var n uint32
	binary.Read(r, binary.BigEndian, &n)
	if string(cipher) != "none" || n != 1 {
		return nil, fmt.Errorf("%s: want one unencrypted key (cipher %q, %d keys)", path, cipher, n)
	}
	str(r) // public key
	private, err := str(r)
	if err != nil {
		return nil, fmt.Errorf("%s: truncated", path)
	}

	pr := bytes.NewReader(private)
	var check [2]uint32
	binary.Read(pr, binary.BigEndian, &check)
	kind, _ := str(pr)
	str(pr) // public half
	key, err := str(pr)
	if check[0] != check[1] || string(kind) != "ssh-ed25519" || err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return File(key), nil
}

func str(r *bytes.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || int64(n) > int64(r.Len()) {
		return nil, errors.New("truncated")
	}
	b := make([]byte, n)
	io.ReadFull(r, b)
	return b, nil
}
