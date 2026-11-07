// Package catalog checks and fetches the signed template catalog published by
// github.com/melzi-cloud/bakery (task #16, D-035).
package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"aead.dev/minisign"
)

// PublicKeys are the minisign keys whose signatures sb-control accepts. To rotate: add the
// new key here, ship it, then sign with the new key and remove the old one a release later.
var PublicKeys = []string{
	"RWTq5XOdoMhS0emtzQd35R9xM8zrXS6lhJO+1zAwwbrjFmsVx6pa5K1D", // D152C8A09D73E5EA, bakery CI, 2026-11-07
}

// DefaultURL is where the latest published catalog lives; the signature is at URL + ".minisig".
const DefaultURL = "https://github.com/melzi-cloud/bakery/releases/latest/download/catalog.json"

// MaxSize bounds a downloaded catalog.
const MaxSize = 8 << 20

// Verify checks that sig is a valid minisign signature of data by one of keys
// (PublicKeys when keys is nil).
func Verify(data, sig []byte, keys []string) error {
	if keys == nil {
		keys = PublicKeys
	}
	if len(sig) == 0 {
		return errors.New("catalog has no signature")
	}
	for _, k := range keys {
		var pk minisign.PublicKey
		if err := pk.UnmarshalText([]byte(k)); err != nil {
			return fmt.Errorf("bad built-in public key: %w", err)
		}
		if minisign.Verify(pk, data, sig) {
			return nil
		}
	}
	return errors.New("catalog signature does not match any trusted key")
}

// Fetch downloads a catalog and its signature.
func Fetch(ctx context.Context, url string) (data, sig []byte, err error) {
	if url == "" {
		url = DefaultURL
	}
	c := &http.Client{Timeout: 60 * time.Second}
	get := func(u string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize+1))
		if err != nil {
			return nil, err
		}
		if len(b) > MaxSize {
			return nil, fmt.Errorf("GET %s: larger than %d bytes", u, MaxSize)
		}
		return b, nil
	}
	if data, err = get(url); err != nil {
		return nil, nil, err
	}
	if sig, err = get(url + ".minisig"); err != nil {
		return nil, nil, err
	}
	return data, sig, nil
}
