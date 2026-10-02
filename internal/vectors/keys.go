// Package vectors generates the golden vectors under testdata/vectors from
// the codec in borgrec. Every value is derived from fixed stand-ins
// (public test keys, hashes of labels for anything a client draws at
// random), so the files are reproducible byte for byte; every check the
// generator makes (a wallet decrypts what was wrapped, a certificate
// verifies, the SDK still refuses a short protocol name) fails the run when
// it stops holding.
package vectors

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// Hall is the vectors' hall identifier; its suffix is a fixed example.
const Hall = "example_hall_vkqzmtrwxp"

// Created is the vectors' fixed time, 2027-01-01T00:00:00Z.
const Created = 1798761600

// Party is one public test identity: 32 bytes of one value as the private
// key. Never for real use.
type Party struct {
	Name string
	Priv *ec.PrivateKey
}

func party(name string, b byte) Party {
	k, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{b}, 32))
	return Party{Name: name, Priv: k}
}

// The parties: the organisation and three members.
var (
	Org   = party("org", 0x51)
	Alice = party("alice", 0x52)
	Bob   = party("bob", 0x53)
	Carol = party("carol", 0x54)
	// Dave is never a member: the stranger of refusal cases.
	Dave = party("dave", 0x56)
)

// Pub is the party's compressed identity key.
func (p Party) Pub() []byte { return p.Priv.PubKey().Compressed() }

// Wallet is a BRC-100 wallet over the party's key.
func (p Party) Wallet() wallet.Interface {
	w, err := wallet.NewCompletedProtoWallet(p.Priv)
	if err != nil {
		panic(err)
	}
	return w
}

// label is the fixed stand-in for a random value: SHA-256 of a label.
func label(format string, a ...any) [32]byte {
	return sha256.Sum256([]byte("borg/vector/" + fmt.Sprintf(format, a...)))
}

// GroupID is the vectors' group identifier: the first 16 bytes of a label.
func GroupID() [16]byte {
	l := label("group/eng")
	var id [16]byte
	copy(id[:], l[:16])
	return id
}

func hx(b []byte) string { return hex.EncodeToString(b) }

// displayHex writes a hash-byte-order txid in display order.
func displayHex(h [32]byte) string {
	r := make([]byte, 32)
	for i := range h {
		r[i] = h[31-i]
	}
	return hx(r)
}
