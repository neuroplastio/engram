package store

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/neuroplastio/engram/sigv4"
)

// CloudFront purges paths from a CloudFront distribution in front of a bucket.
//
// A build's files are served as immutable, which is what makes them cheap: an
// edge that has one never asks again. It is also why deleting a build from the
// bucket does not stop it being served — and for a build withdrawn because it
// is dangerous, that is the whole point of withdrawing it. So the one time a
// cache is invalidated is when a build's files go.
type CloudFront struct {
	DistributionID string
	Credentials    sigv4.Credentials
	HTTP           *http.Client
}

// Purge invalidates everything under prefix, which must end in "/".
func (c *CloudFront) Purge(ctx context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("cloudfront: a prefix to purge must end in /")
	}
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><InvalidationBatch xmlns="http://cloudfront.amazonaws.com/doc/2020-05-31/"><Paths><Quantity>1</Quantity><Items><Path>`)
	xml.EscapeText(&body, []byte("/"+strings.TrimPrefix(prefix, "/")+"*"))
	fmt.Fprintf(&body, `</Path></Items></Paths><CallerReference>engram-%d</CallerReference></InvalidationBatch>`, time.Now().UnixNano())

	url := "https://cloudfront.amazonaws.com/2020-05-31/distribution/" + c.DistributionID + "/invalidation"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/xml")
	// CloudFront is a global service signed for us-east-1.
	if err := sigv4.Sign(req, c.Credentials, "us-east-1", "cloudfront", time.Now()); err != nil {
		return err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Code    string `xml:"Error>Code"`
			Message string `xml:"Error>Message"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		xml.Unmarshal(data, &e)
		return fmt.Errorf("cloudfront: %s %s %s", resp.Status, e.Code, e.Message)
	}
	return nil
}
