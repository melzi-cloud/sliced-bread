//go:build e2e

package hetzner

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// A minimal S3 client (signature v4, path-style) for removing a test's backup prefix.

type s3Client struct {
	endpoint, region, bucket, key, secret string
}

func s3Escape(s string, slash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func (c s3Client) do(ctx context.Context, method, key string, q url.Values) ([]byte, error) {
	u, err := url.Parse(strings.TrimRight(c.endpoint, "/"))
	if err != nil {
		return nil, err
	}
	path := "/" + c.bucket
	if key != "" {
		path += "/" + s3Escape(key, false)
	}
	var qs []string
	for k, vs := range q {
		for _, v := range vs {
			qs = append(qs, s3Escape(k, true)+"="+s3Escape(v, true))
		}
	}
	sort.Strings(qs)
	query := strings.Join(qs, "&")
	now := time.Now().UTC()
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	empty := hex.EncodeToString(func() []byte { h := sha256.Sum256(nil); return h[:] }())
	canon := strings.Join([]string{method, path, query,
		"host:" + u.Host + "\nx-amz-content-sha256:" + empty + "\nx-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date", empty}, "\n")
	scope := day + "/" + c.region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(canon))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	k := hmacSHA([]byte("AWS4"+c.secret), day)
	k = hmacSHA(k, c.region)
	k = hmacSHA(k, "s3")
	k = hmacSHA(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	full := u.Scheme + "://" + u.Host + path
	if query != "" {
		full += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", empty)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.key+"/"+scope+", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+sig)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return body, nil
}

// deletePrefix removes every object under prefix and returns how many it removed.
func (c s3Client) deletePrefix(ctx context.Context, prefix string) (int, error) {
	n, token := 0, ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		b, err := c.do(ctx, "GET", "", q)
		if err != nil {
			return n, err
		}
		var res struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(b, &res); err != nil {
			return n, err
		}
		for _, o := range res.Contents {
			if !strings.HasPrefix(o.Key, prefix) {
				continue
			}
			if _, err := c.do(ctx, "DELETE", o.Key, nil); err != nil {
				return n, err
			}
			n++
		}
		if !res.IsTruncated || res.Next == "" {
			return n, nil
		}
		token = res.Next
	}
}
