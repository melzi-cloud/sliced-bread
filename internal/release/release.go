// Package release fetches and checks artifacts of SlicedBread releases on GitHub.
// Release assets are listed in SHA256SUMS, which is signed with the release key
// (minisign, separate from the template catalog key).
package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/catalog"
)

// PublicKeys are the keys whose signatures on SHA256SUMS are accepted.
var PublicKeys = []string{
	"RWTWEJXhA0VVrEhLJXOwGwqCydkC64znIxzL3NqduaYT7UBbvePx9L0i", // AC554503E19510D6, release CI, 2026-11-07
}

// Base is where release assets are downloaded from.
var Base = "https://github.com/melzi-cloud/sliced-bread/releases/download/"

var versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// IsRelease reports whether v names a published release.
func IsRelease(v string) bool { return versionRE.MatchString(v) }

// ControlAsset is the release asset holding the sb-control binary.
const ControlAsset = "sb-control-linux-amd64"

func get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("GET %s: too large", url)
	}
	return b, nil
}

// Sums parses SHA256SUMS lines ("<hex>  <name>").
func Sums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	return out
}

// Fetch downloads one asset of release version and checks it against the signed
// SHA256SUMS of that release.
func Fetch(ctx context.Context, version, asset string) ([]byte, error) {
	if !IsRelease(version) {
		return nil, fmt.Errorf("%q is not a release version", version)
	}
	base := Base + version + "/"
	sums, err := get(ctx, base+"SHA256SUMS", 1<<20)
	if err != nil {
		return nil, err
	}
	sig, err := get(ctx, base+"SHA256SUMS.minisig", 1<<16)
	if err != nil {
		return nil, err
	}
	if err := catalog.Verify(sums, sig, PublicKeys); err != nil {
		return nil, fmt.Errorf("release %s: SHA256SUMS: %w", version, err)
	}
	want, ok := Sums(sums)[asset]
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", version, asset)
	}
	data, err := get(ctx, base+asset, 200<<20)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("release %s: %s does not match its checksum", version, asset)
	}
	return data, nil
}
