// Package cryptoutil provides real X25519 key generation using the
// standard library's crypto/ecdh. There is no mock here — the keys
// produced are usable as-is by quiccochet for mutual authentication.
package cryptoutil

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// KeyPair holds a freshly-generated X25519 key pair. Both keys are
// returned base64-encoded (raw, no padding stripping) which is the
// format quiccochet's config files and admin tools use.
type KeyPair struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// GenerateKeyPair returns a new X25519 key pair drawn from crypto/rand.
// Errors at this point would indicate a kernel RNG failure, which is
// catastrophic — callers should treat them as fatal.
func GenerateKeyPair() (KeyPair, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("x25519 generate: %w", err)
	}
	return KeyPair{
		PrivateKey: base64.StdEncoding.EncodeToString(priv.Bytes()),
		PublicKey:  base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()),
	}, nil
}

// DerivePublic recovers the public half of an existing private key.
// Useful when an admin imports a private key and we need to show them
// the matching public key to share with a peer.
func DerivePublic(privBase64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(privBase64)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}
