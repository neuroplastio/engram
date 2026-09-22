package store

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neuroplastio/engram/sigv4"
)

// S3 is a Store in a bucket of any S3-compatible service: AWS, R2, MinIO, B2.
// It is four requests over plain HTTPS, signed by sigv4 — no SDK.
//
// The journal's safety rests on conditional writes (If-Match and
// If-None-Match on PUT). AWS S3, R2 and MinIO honour them; a service that
// ignores them would let two concurrent publishers overwrite each other.
type S3 struct {
	Bucket string
	Region string // "auto" or anything for services that do not care

	// Endpoint is the service URL for anything that is not AWS, e.g.
	// https://<account>.r2.cloudflarestorage.com. Buckets are then addressed
	// by path. Empty means AWS, addressed by host.
	Endpoint string

	Credentials sigv4.Credentials
	HTTP        *http.Client
}

func (s *S3) url(key string, query url.Values) string {
	var base string
	if s.Endpoint != "" {
		base = strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket
	} else {
		base = "https://" + s.Bucket + ".s3." + s.Region + ".amazonaws.com"
	}
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = sigv4.Escape(seg)
	}
	u := base + "/" + strings.Join(segs, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (s *S3) do(ctx context.Context, method, url string, body []byte, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if err := sigv4.Sign(req, s.Credentials, s.Region, "s3", time.Now()); err != nil {
		return nil, err
	}
	hc := s.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// apiError turns a non-2xx response into an error carrying the service's code.
func apiError(resp *http.Response) error {
	defer resp.Body.Close()
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	xml.Unmarshal(data, &e)
	if e.Code == "" {
		return fmt.Errorf("s3: %s", resp.Status)
	}
	return fmt.Errorf("s3: %s: %s", e.Code, e.Message)
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, string, error) {
	resp, err := s.do(ctx, http.MethodGet, s.url(key, nil), nil, nil)
	if err != nil {
		return nil, "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return data, resp.Header.Get("ETag"), err
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, "", ErrNotFound
	}
	return nil, "", apiError(resp)
}

func (s *S3) Put(ctx context.Context, key string, data []byte, opts PutOptions) error {
	h := http.Header{}
	for name, v := range map[string]string{"Cache-Control": opts.CacheControl, "Content-Type": opts.ContentType, "If-Match": opts.IfMatch} {
		if v != "" {
			h.Set(name, v)
		}
	}
	if opts.IfAbsent {
		h.Set("If-None-Match", "*")
	}
	resp, err := s.do(ctx, http.MethodPut, s.url(key, nil), data, h)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		resp.Body.Close()
		return nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		resp.Body.Close()
		return ErrConflict
	}
	return apiError(resp)
}

// Copy is S3's CopyObject: a PUT that names its source. REPLACE makes the
// copy carry opts rather than the source's headers — a mirror of an
// immutable file must not be served as immutable.
func (s *S3) Copy(ctx context.Context, src, dst string, opts PutOptions) error {
	h := http.Header{}
	for name, v := range map[string]string{"Cache-Control": opts.CacheControl, "Content-Type": opts.ContentType} {
		if v != "" {
			h.Set(name, v)
		}
	}
	segs := strings.Split(src, "/")
	for i, seg := range segs {
		segs[i] = sigv4.Escape(seg)
	}
	h.Set("x-amz-copy-source", "/"+s.Bucket+"/"+strings.Join(segs, "/"))
	h.Set("x-amz-metadata-directive", "REPLACE")
	resp, err := s.do(ctx, http.MethodPut, s.url(dst, nil), nil, h)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	// A copy that fails part way answers 200 with an error in the body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if bytes.Contains(body, []byte("<Error>")) {
		return fmt.Errorf("s3: copy %s: %s", src, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, s.url(key, nil), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	}
	return apiError(resp)
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") {
		return errors.New("store: a prefix to delete must end in /")
	}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, s.url("", q), nil, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return apiError(resp)
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("s3: list: %w", err)
		}
		// One request per key. A build is a dozen files; the batch API would
		// cost more code than it saves requests.
		for _, o := range page.Contents {
			resp, err := s.do(ctx, http.MethodDelete, s.url(o.Key, nil), nil, nil)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
				return apiError(resp)
			}
			resp.Body.Close()
		}
		if !page.IsTruncated {
			return nil
		}
		token = page.Next
	}
}
