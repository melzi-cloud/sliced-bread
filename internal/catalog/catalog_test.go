package catalog

import (
	"crypto/rand"
	"testing"

	"aead.dev/minisign"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

func TestEmbeddedSnapshotIsSigned(t *testing.T) {
	data, sig := apps.Snapshot()
	if err := Verify(data, sig, nil); err != nil {
		t.Fatalf("the snapshot built into sb-control must verify with the built-in keys: %v", err)
	}
	if apps.SnapshotErr != nil || apps.CurrentCatalog().Serial == 0 || apps.CatalogTemplate("vaultwarden") == nil {
		t.Fatal("snapshot not loaded")
	}
}

func TestVerifyRefusesTamperingAndOtherKeys(t *testing.T) {
	pub, priv, _ := minisign.GenerateKey(rand.Reader)
	pubText, _ := pub.MarshalText()
	keys := []string{string(pubText)}
	data := []byte(`{"format":1,"serial":5}`)
	sig := minisign.Sign(priv, data)
	if err := Verify(data, sig, keys); err != nil {
		t.Fatal(err)
	}
	if Verify([]byte(`{"format":1,"serial":6}`), sig, keys) == nil {
		t.Fatal("changed data accepted")
	}
	if Verify(data, nil, keys) == nil {
		t.Fatal("missing signature accepted")
	}
	if Verify(data, sig, nil) == nil {
		t.Fatal("a signature by a key sb-control does not trust was accepted")
	}
	// A real snapshot signature does not carry over to other data.
	_, realSig := apps.Snapshot()
	if Verify(data, realSig, nil) == nil {
		t.Fatal("the snapshot signature verified other data")
	}
}
