// Command release signs and checks SHA256SUMS of a SlicedBread release. The release
// workflow runs it; it is not shipped to users.
//
//	release sign SHA256SUMS      private key (base64 of the minisign key text) in $RELEASE_SIGNING_KEY
//	release verify SHA256SUMS    checks SHA256SUMS.minisig against the keys built into sb
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"aead.dev/minisign"

	"github.com/melzi-cloud/sliced-bread/internal/catalog"
	"github.com/melzi-cloud/sliced-bread/internal/release"
)

func main() {
	if len(os.Args) != 3 {
		fail("usage: release sign|verify SHA256SUMS")
	}
	p := os.Args[2]
	data, err := os.ReadFile(p)
	if err != nil {
		fail(err.Error())
	}
	switch os.Args[1] {
	case "sign":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY")))
		if err != nil || len(raw) == 0 {
			fail("RELEASE_SIGNING_KEY is missing or not base64")
		}
		var pk minisign.PrivateKey
		if err := pk.UnmarshalText(raw); err != nil {
			fail("signing key: " + err.Error())
		}
		sig := minisign.SignWithComments(pk, data, "slicedbread release "+os.Getenv("RELEASE_VERSION"), "signature from the SlicedBread release key")
		if err := os.WriteFile(p+".minisig", sig, 0o644); err != nil {
			fail(err.Error())
		}
		fmt.Printf("signed %s with key %X\n", p, pk.ID())
	case "verify":
	default:
		fail("usage: release sign|verify SHA256SUMS")
	}
	sig, err := os.ReadFile(p + ".minisig")
	if err != nil {
		fail(err.Error())
	}
	if err := catalog.Verify(data, sig, release.PublicKeys); err != nil {
		fail(err.Error())
	}
	fmt.Printf("%s: signature ok (%d files)\n", p, len(release.Sums(data)))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "release:", msg)
	os.Exit(1)
}
