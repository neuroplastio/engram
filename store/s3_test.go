package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neuroplastio/engram/sigv4"
)

// A copy is a PUT with no body. S3 refuses a PUT whose body arrives chunked
// (NotImplemented), which is what Go sends for a zero-length reader — so the
// wire shape of a copy is pinned here: an explicit Content-Length of 0, the
// source named, and both S3 headers covered by the signature.
func TestCopyIsAnUnchunkedPutNamingItsSource(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Write([]byte(`<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`))
	}))
	defer srv.Close()
	s := &S3{Bucket: "b", Region: "auto", Endpoint: srv.URL,
		Credentials: sigv4.Credentials{AccessKey: "k", SecretKey: "s"}}
	if err := s.Copy(context.Background(), "p/dev/builds/abc/tool tar.gz", "p/dev/latest/tool tar.gz",
		PutOptions{CacheControl: "public, max-age=60"}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPut || got.URL.EscapedPath() != "/b/p/dev/latest/tool%20tar.gz" {
		t.Errorf("%s %s", got.Method, got.URL)
	}
	if len(got.TransferEncoding) != 0 || got.ContentLength != 0 || got.Header.Get("Content-Length") != "0" {
		t.Errorf("body: transfer-encoding=%v content-length=%d (%q)", got.TransferEncoding, got.ContentLength, got.Header.Get("Content-Length"))
	}
	if src := got.Header.Get("X-Amz-Copy-Source"); src != "/b/p/dev/builds/abc/tool%20tar.gz" {
		t.Errorf("copy source %q", src)
	}
	if got.Header.Get("X-Amz-Metadata-Directive") != "REPLACE" || got.Header.Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("headers %v", got.Header)
	}
	auth := got.Header.Get("Authorization")
	for _, h := range []string{"x-amz-copy-source", "x-amz-metadata-directive"} {
		if !strings.Contains(auth, h) {
			t.Errorf("%s is not signed: %s", h, auth)
		}
	}
}

// A copy that failed part way answers 200 with an error in the body.
func TestCopyReadsTheErrorInsideA200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<Error><Code>InternalError</Code><Message>We encountered an internal error.</Message></Error>`))
	}))
	defer srv.Close()
	s := &S3{Bucket: "b", Region: "auto", Endpoint: srv.URL, Credentials: sigv4.Credentials{AccessKey: "k", SecretKey: "s"}}
	if err := s.Copy(context.Background(), "a", "b", PutOptions{}); err == nil || !strings.Contains(err.Error(), "InternalError") {
		t.Errorf("err = %v", err)
	}
}
