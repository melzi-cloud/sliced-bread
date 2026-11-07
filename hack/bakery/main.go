// Command bakery builds, signs and checks the template catalog of
// github.com/melzi-cloud/bakery. Bakery's CI runs it; it is not shipped to users.
//
//	bakery build <templates-dir> -o catalog.json [-source repo@commit]
//	bakery sign catalog.json          private key (base64 of the minisign key text) in $BAKERY_SIGNING_KEY
//	bakery verify catalog.json        checks catalog.json.minisig against sb-control's built-in keys
//	bakery fetch [url] -o dir         downloads the published catalog, verifies it, writes both files
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aead.dev/minisign"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/catalog"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bakery:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bakery build|sign|verify|fetch ...")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	out := fs.String("o", "", "output")
	source := fs.String("source", "", "repository@commit the catalog is built from")
	serial := fs.Int64("serial", 0, "catalog serial (default: now, in Unix seconds)")
	// Flags may come after the positional argument.
	var pos []string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos, rest = append(pos, rest[0]), rest[1:]
		}
	}
	switch args[0] {
	case "build":
		if len(pos) != 1 || *out == "" {
			return fmt.Errorf("usage: bakery build <templates-dir> -o catalog.json")
		}
		now := time.Now()
		if *serial == 0 {
			*serial = now.Unix()
		}
		b, err := apps.BuildBundle(os.DirFS(pos[0]), *serial, now, *source)
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(b, "", " ")
		if _, _, _, err := apps.LoadBundle(data); err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return err
		}
		for _, t := range b.Templates {
			fmt.Printf("%s %s (%s)\n", t.Name, t.Version, t.Hash[:12])
		}
		fmt.Printf("catalog serial %d, %d templates -> %s\n", b.Serial, len(b.Templates), *out)
	case "sign":
		if len(pos) != 1 {
			return fmt.Errorf("usage: bakery sign catalog.json")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("BAKERY_SIGNING_KEY")))
		if err != nil || len(raw) == 0 {
			return fmt.Errorf("BAKERY_SIGNING_KEY is missing or not base64")
		}
		var pk minisign.PrivateKey
		if err := pk.UnmarshalText(raw); err != nil {
			return fmt.Errorf("signing key: %v", err)
		}
		data, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		var b apps.Bundle
		json.Unmarshal(data, &b)
		sig := minisign.SignWithComments(pk, data, fmt.Sprintf("slicedbread catalog serial %d", b.Serial), "signature from the bakery catalog key")
		if err := os.WriteFile(pos[0]+".minisig", sig, 0o644); err != nil {
			return err
		}
		if err := catalog.Verify(data, sig, nil); err != nil {
			return fmt.Errorf("signed, but the signature does not verify against sb-control's built-in keys: %v", err)
		}
		fmt.Printf("signed %s with key %X\n", pos[0], pk.ID())
	case "verify":
		if len(pos) != 1 {
			return fmt.Errorf("usage: bakery verify catalog.json")
		}
		return verifyFiles(pos[0])
	case "fetch":
		url := catalog.DefaultURL
		if len(pos) == 1 {
			url = pos[0]
		}
		if *out == "" {
			return fmt.Errorf("usage: bakery fetch [url] -o <dir>")
		}
		data, sig, err := catalog.Fetch(context.Background(), url)
		if err != nil {
			return err
		}
		if err := catalog.Verify(data, sig, nil); err != nil {
			return err
		}
		p := filepath.Join(*out, "catalog.json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(p+".minisig", sig, 0o644); err != nil {
			return err
		}
		return verifyFiles(p)
	default:
		return fmt.Errorf("unknown command %s", args[0])
	}
	return nil
}

func verifyFiles(p string) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(p + ".minisig")
	if err != nil {
		return err
	}
	if err := catalog.Verify(data, sig, nil); err != nil {
		return err
	}
	b, tmpls, skipped, err := apps.LoadBundle(data)
	if err != nil {
		return err
	}
	fmt.Printf("%s: signature ok, serial %d, built %s from %s, %d templates usable\n", p, b.Serial, b.Built, b.Source, len(tmpls))
	for _, s := range skipped {
		fmt.Println("  skipped:", s)
	}
	return nil
}
