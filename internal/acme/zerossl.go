// Package acme holds helpers for ACME certificate authorities.
package acme

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ZeroSSLDirectory is ZeroSSL's ACME directory.
const ZeroSSLDirectory = "https://acme.zerossl.com/v2/DV90"

// ZeroSSLEAB requests ACME external account binding credentials from ZeroSSL for an email
// address, the same way acme.sh and Caddy do. The credentials are reusable.
func ZeroSSLEAB(ctx context.Context, email string) (kid, hmac string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.zerossl.com/acme/eab-credentials-email",
		strings.NewReader(url.Values{"email": {email}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var r struct {
		Success bool   `json:"success"`
		KID     string `json:"eab_kid"`
		HMAC    string `json:"eab_hmac_key"`
		Error   any    `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return "", "", fmt.Errorf("unexpected response (HTTP %d)", resp.StatusCode)
	}
	if !r.Success || r.KID == "" || r.HMAC == "" {
		return "", "", fmt.Errorf("no credentials returned: %v", r.Error)
	}
	return r.KID, r.HMAC, nil
}
