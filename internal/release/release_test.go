package release

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aead.dev/minisign"
)

func TestFetchChecksSignatureAndChecksum(t *testing.T) {
	pub, priv, _ := minisign.GenerateKey(rand.Reader)
	_, other, _ := minisign.GenerateKey(rand.Reader)
	pubText, _ := pub.MarshalText()
	savedKeys, savedBase := PublicKeys, Base
	defer func() { PublicKeys, Base = savedKeys, savedBase }()
	PublicKeys = []string{string(pubText)}

	bin := []byte("the sb-control binary")
	h := sha256.Sum256(bin)
	sums := []byte(hex.EncodeToString(h[:]) + "  " + ControlAsset + "\n")
	files := map[string][]byte{
		"/v0.1.0/SHA256SUMS":         sums,
		"/v0.1.0/SHA256SUMS.minisig": minisign.Sign(priv, sums),
		"/v0.1.0/" + ControlAsset:    bin,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	Base = srv.URL + "/"
	ctx := context.Background()

	if got, err := Fetch(ctx, "v0.1.0", ControlAsset); err != nil || string(got) != string(bin) {
		t.Fatalf("good release: %q %v", got, err)
	}
	files["/v0.1.0/"+ControlAsset] = []byte("swapped binary")
	if _, err := Fetch(ctx, "v0.1.0", ControlAsset); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("swapped binary accepted: %v", err)
	}
	files["/v0.1.0/"+ControlAsset] = bin
	files["/v0.1.0/SHA256SUMS.minisig"] = minisign.Sign(other, sums)
	if _, err := Fetch(ctx, "v0.1.0", ControlAsset); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("checksums signed by another key accepted: %v", err)
	}
	if _, err := Fetch(ctx, "dev", ControlAsset); err == nil {
		t.Fatal("dev is not a release")
	}
	if !IsRelease("v1.2.3") || !IsRelease("v0.1.0-rc.1") || IsRelease("1.2.3") || IsRelease("v1.2") {
		t.Fatal("IsRelease")
	}
}
