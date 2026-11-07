package control

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"aead.dev/minisign"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/catalog"
)

// signedVariant returns the embedded catalog with a new serial (and an optional change to
// one template), signed by a throwaway key that the test trusts.
func signedVariant(t *testing.T, priv minisign.PrivateKey, serial int64, mutate func(b *apps.Bundle)) ([]byte, []byte) {
	t.Helper()
	data, _ := apps.Snapshot()
	var b apps.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	b.Serial = serial
	if mutate != nil {
		mutate(&b)
	}
	out, _ := json.Marshal(b)
	return out, minisign.Sign(priv, out)
}

func TestInstallCatalog(t *testing.T) {
	pub, priv, _ := minisign.GenerateKey(rand.Reader)
	pubText, _ := pub.MarshalText()
	saved, savedInfo := catalog.PublicKeys, apps.CurrentCatalog()
	catalog.PublicKeys = []string{string(pubText)}
	defer func() {
		catalog.PublicKeys = saved
		data, _ := apps.Snapshot()
		_, tm, _, _ := apps.LoadBundle(data)
		apps.SetCatalog(savedInfo, tm)
	}()
	base := apps.CurrentCatalog().Serial

	// Newer and signed: installed.
	data, sig := signedVariant(t, priv, base+10, func(b *apps.Bundle) {
		for i := range b.Templates {
			if b.Templates[i].Name == "umami" {
				e := &b.Templates[i]
				e.Files["template.yaml"] = strings.Replace(e.Files["template.yaml"], "version: 1.0.0", "version: 1.0.1", 1)
				e.Version = "1.0.1"
				e.Hash = apps.TestFilesHash(e.Files)
			}
		}
	})
	res, err := installCatalog(data, sig, "refreshed")
	if err != nil || !res.Changed || apps.CatalogTemplate("umami").Version != "1.0.1" {
		t.Fatalf("newer catalog not installed: %+v %v", res, err)
	}
	// Same serial again: no change.
	if res, err := installCatalog(data, sig, "refreshed"); err != nil || res.Changed {
		t.Fatalf("same serial: %+v %v", res, err)
	}
	// Older: refused (a replayed old catalog cannot roll templates back).
	old, oldSig := signedVariant(t, priv, base+5, nil)
	if _, err := installCatalog(old, oldSig, "refreshed"); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("older catalog accepted: %v", err)
	}
	// Tampered after signing: refused.
	tampered := []byte(strings.Replace(string(data), "1.0.1", "6.6.6", 1))
	if _, err := installCatalog(tampered, sig, "refreshed"); err == nil {
		t.Fatal("tampered catalog accepted")
	}
	// Signed, but a file does not match its hash: refused.
	bad, badSig := signedVariant(t, priv, base+20, func(b *apps.Bundle) {
		b.Templates[0].Files["template.yaml"] += "\n# changed"
	})
	if _, err := installCatalog(bad, badSig, "refreshed"); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("hash mismatch accepted: %v", err)
	}
	if apps.CurrentCatalog().Serial != base+10 {
		t.Fatal("a refused catalog changed the one in use")
	}
	// Other tiers are not offered at launch.
	comm, commSig := signedVariant(t, priv, base+30, func(b *apps.Bundle) { b.Templates[0].Tier = "community" })
	res, err = installCatalog(comm, commSig, "refreshed")
	if err != nil || len(res.Skipped) != 1 || apps.CatalogTemplate(b0name(comm)) != nil {
		t.Fatalf("community template listed: %+v %v", res, err)
	}
}

func b0name(data []byte) string {
	var b apps.Bundle
	json.Unmarshal(data, &b)
	return b.Templates[0].Name
}
