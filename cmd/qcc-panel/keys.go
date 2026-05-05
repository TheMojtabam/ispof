package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/curve25519"
)

func runGenKey() {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		fmt.Fprintf(os.Stderr, "rand: %v\n", err)
		os.Exit(1)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	fmt.Println(base64.StdEncoding.EncodeToString(priv))
}

func runPubKey() {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stdin: %v\n", err)
		os.Exit(1)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(in)))
	if err != nil || len(priv) != 32 {
		fmt.Fprintln(os.Stderr, "invalid private key")
		os.Exit(1)
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "x25519: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
}
