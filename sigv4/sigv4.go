// Package sigv4 signs HTTP requests with AWS Signature Version 4 — the scheme
// S3 and every S3-compatible store speaks, and KMS too. It exists so that
// engram needs no cloud SDK: the two services it talks to are a handful of
// plain HTTPS requests, and this is the only part of them that is not obvious.
package sigv4

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Credentials are short-lived or long-lived keys. Token is empty for the
// latter.
type Credentials struct {
	AccessKey, SecretKey, Token string
}

// FromEnv reads the standard variables. That is the whole credential chain, on
// purpose: in CI, aws-actions/configure-aws-credentials exports exactly these
// from an OIDC role; on a workstation,
//
//	eval "$(aws configure export-credentials --format env)"
//
// does the same from whatever session is signed in.
func FromEnv() (Credentials, error) {
	c := Credentials{
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Token:     os.Getenv("AWS_SESSION_TOKEN"),
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return c, errors.New("no credentials: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (and AWS_SESSION_TOKEN for a session)")
	}
	return c, nil
}

// Sign adds the authorization headers to req. The body is read and replaced,
// because its hash is part of what is signed.
func Sign(req *http.Request, c Credentials, region, service string, now time.Time) error {
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	payload := hexHash(body)
	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if c.Token != "" {
		req.Header.Set("X-Amz-Security-Token", c.Token)
	}

	// Signed: the host and every x-amz-* header. The rest may be rewritten by a
	// proxy on the way and must not break the signature.
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")

	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hexHash([]byte(canonical))

	key := mac([]byte("AWS4"+c.SecretKey), day)
	key = mac(key, region)
	key = mac(key, service)
	key = mac(key, "aws4_request")

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(mac(key, toSign)))
	return nil
}

// canonicalPath encodes each segment once and keeps the slashes — S3's rule.
func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		segs[i] = Escape(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, Escape(k)+"="+Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

// Escape is RFC 3986 percent-encoding: everything but unreserved characters.
// net/url's escapers each leave something else alone, and a signature does not
// survive the difference.
func Escape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
