// Package dns is a minimal Cloudflare DNS client for development tooling.
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Record is a Cloudflare DNS record.
type Record struct {
	ID         string    `json:"id,omitempty"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	TTL        int       `json:"ttl"`
	Proxied    bool      `json:"proxied"`
	Comment    string    `json:"comment,omitempty"`
	CreatedOn  time.Time `json:"created_on,omitempty"`
	ModifiedOn time.Time `json:"modified_on,omitempty"`
}

// Client talks to the Cloudflare v4 API. The token is never logged.
type Client struct {
	token   string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client using the given API token.
func New(token string) *Client {
	return &Client{token: token, BaseURL: "https://api.cloudflare.com/client/v4", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []apiError      `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (envelope, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return envelope{}, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("cloudflare %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return envelope{}, fmt.Errorf("cloudflare %s %s: HTTP %d, undecodable body: %w", method, path, resp.StatusCode, err)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return env, fmt.Errorf("cloudflare %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.Join(msgs, "; "))
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return env, fmt.Errorf("cloudflare %s %s: decode result: %w", method, path, err)
		}
	}
	return env, nil
}

// ZoneID looks up a zone id by name.
func (c *Client) ZoneID(ctx context.Context, name string) (string, error) {
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
		return "", err
	}
	for _, z := range zones {
		if z.Name == name {
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("zone %q not visible to this token", name)
}

// Create creates a record and returns it with its id.
func (c *Client) Create(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out Record
	_, err := c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", r, &out)
	return out, err
}

// Delete removes a record. Deleting a record that is already gone is not an error.
func (c *Client) Delete(ctx context.Context, zoneID, recordID string) error {
	env, err := c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
	if err != nil {
		for _, e := range env.Errors {
			if e.Code == 81044 { // record does not exist
				return nil
			}
		}
	}
	return err
}

// ListUnder returns all records whose name equals suffix or ends with "."+suffix.
func (c *Client) ListUnder(ctx context.Context, zoneID, suffix string) ([]Record, error) {
	var all []Record
	for page := 1; ; page++ {
		var recs []Record
		env, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?per_page=100&page=%d", zoneID, page), nil, &recs)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.Name == suffix || strings.HasSuffix(r.Name, "."+suffix) {
				all = append(all, r)
			}
		}
		if env.ResultInfo.TotalPages == 0 || page >= env.ResultInfo.TotalPages {
			return all, nil
		}
	}
}

// Zone is a Cloudflare zone visible to the token.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Zones lists every zone the token can see.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	var all []Zone
	for page := 1; ; page++ {
		var zs []Zone
		env, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones?per_page=50&page=%d", page), nil, &zs)
		if err != nil {
			return nil, err
		}
		all = append(all, zs...)
		if env.ResultInfo.TotalPages == 0 || page >= env.ResultInfo.TotalPages {
			return all, nil
		}
	}
}

// ZoneFor finds the most specific zone that contains domain.
func (c *Client) ZoneFor(ctx context.Context, domain string) (Zone, error) {
	zs, err := c.Zones(ctx)
	if err != nil {
		return Zone{}, err
	}
	var best Zone
	for _, z := range zs {
		if (domain == z.Name || strings.HasSuffix(domain, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	if best.ID == "" {
		names := make([]string, 0, len(zs))
		for _, z := range zs {
			names = append(names, z.Name)
		}
		return Zone{}, fmt.Errorf("no Cloudflare zone visible to this token contains %s (token sees: %s)", domain, strings.Join(names, ", "))
	}
	return best, nil
}
