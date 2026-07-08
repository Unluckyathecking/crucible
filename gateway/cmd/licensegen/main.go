// Command licensegen mints and inspects Crucible deployment license keys.
//
//	licensegen keygen
//	licensegen sign --priv <hex> --licensee "Acme" --email a@b.com --edition pro [--features a,b] [--seats N] [--expires 2027-01-01]
//	licensegen verify --pub <hex> --key cru1....
//
// keygen prints a fresh Ed25519 keypair. Keep the private key OFFLINE; embed the
// public key in license.DefaultPublicKeyHex (or set CRUCIBLE_LICENSE_PUBKEY).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/license"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "licensegen: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  licensegen keygen
  licensegen sign --priv <hex> --licensee <name> --email <email> --edition pro|business|enterprise [--features a,b] [--seats N] [--expires YYYY-MM-DD]
  licensegen verify --pub <hex> --key <license>
`)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Printf("private (KEEP OFFLINE): %s\n", hex.EncodeToString(priv.Seed()))
	fmt.Printf("public  (embed as DefaultPublicKeyHex / CRUCIBLE_LICENSE_PUBKEY): %s\n", hex.EncodeToString(pub))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	privHex := fs.String("priv", "", "hex-encoded ed25519 private key (seed or full)")
	licensee := fs.String("licensee", "", "licensee name")
	email := fs.String("email", "", "licensee email")
	edition := fs.String("edition", "", "pro|business|enterprise")
	featuresCSV := fs.String("features", "", "comma-separated feature overrides (empty = edition defaults)")
	seats := fs.Int("seats", 1, "seat count")
	expires := fs.String("expires", "", "expiry date YYYY-MM-DD (default: 1 year from now)")
	id := fs.String("id", "", "license id (default: generated lic_<random>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privHex == "" || *licensee == "" || *email == "" || *edition == "" {
		return fmt.Errorf("--priv, --licensee, --email, and --edition are required")
	}
	priv, err := privateKeyFromHex(*privHex)
	if err != nil {
		return err
	}

	issued := time.Now().UTC()
	exp := issued.AddDate(1, 0, 0)
	if *expires != "" {
		exp, err = time.Parse("2006-01-02", *expires)
		if err != nil {
			return fmt.Errorf("--expires must be YYYY-MM-DD: %w", err)
		}
	}

	var features []string
	if *featuresCSV != "" {
		for _, f := range strings.Split(*featuresCSV, ",") {
			if f = strings.TrimSpace(f); f != "" {
				features = append(features, f)
			}
		}
	}

	licID := *id
	if licID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		licID = "lic_" + hex.EncodeToString(b[:])
	}

	key, err := license.Sign(license.SignInput{
		ID:        licID,
		Licensee:  *licensee,
		Email:     *email,
		Edition:   *edition,
		Features:  features,
		Seats:     *seats,
		IssuedAt:  issued,
		ExpiresAt: exp,
	}, priv)
	if err != nil {
		return err
	}
	fmt.Println(key)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pubHex := fs.String("pub", "", "hex-encoded ed25519 public key")
	key := fs.String("key", "", "license key (cru1....)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pubHex == "" || *key == "" {
		return fmt.Errorf("--pub and --key are required")
	}
	pub, err := license.ResolvePublicKey(*pubHex)
	if err != nil {
		return err
	}
	lic, err := license.Parse(*key, pub)
	if err != nil {
		return err
	}
	fmt.Printf("id:         %s\n", lic.ID)
	fmt.Printf("licensee:   %s\n", lic.Licensee)
	fmt.Printf("email:      %s\n", lic.Email)
	fmt.Printf("edition:    %s\n", lic.Edition)
	fmt.Printf("features:   %s\n", strings.Join(lic.Features, ", "))
	fmt.Printf("seats:      %d\n", lic.Seats)
	fmt.Printf("issued_at:  %s\n", lic.IssuedAt.Format(time.RFC3339))
	fmt.Printf("expires_at: %s\n", lic.ExpiresAt.Format(time.RFC3339))
	fmt.Printf("in_grace:   %t\n", lic.InGrace())
	return nil
}

// privateKeyFromHex accepts either a 32-byte seed (what keygen prints) or a full
// 64-byte private key, and returns the full ed25519.PrivateKey.
func privateKeyFromHex(h string) (ed25519.PrivateKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil {
		return nil, fmt.Errorf("decode private key hex: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("private key must be %d (seed) or %d bytes (got %d)", ed25519.SeedSize, ed25519.PrivateKeySize, len(raw))
	}
}
