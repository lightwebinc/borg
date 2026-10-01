package vectors

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/borg/internal/borgrec"
)

// tsFile is testdata/ts/ts-v1.json, written by the TypeScript codec.
type tsFile struct {
	Hall     string `json:"hall"`
	HallDraw struct {
		Name   string `json:"name"`
		Random string `json:"random"`
		Hall   string `json:"hall"`
	} `json:"hallDraw"`
	EpochKey    string                       `json:"epochKey"`
	Records     map[string]string            `json:"records"`
	GrantValues map[string]map[string]string `json:"grantValues"`
	Headers     []struct {
		Height     uint32 `json:"height"`
		MerkleRoot string `json:"merkleRoot"`
	} `json:"headers"`
	Transactions []struct {
		Name string `json:"name"`
		Txid string `json:"txid"`
		BEEF string `json:"beef"`
		Kind string `json:"kind"`
	} `json:"transactions"`
}

func unhexT(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readTS(t *testing.T) *tsFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/ts", "ts-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f tsFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return &f
}

// TestTSRecords decodes and re-encodes every record the TypeScript codec
// wrote, and draws the same hall from the same bytes.
func TestTSRecords(t *testing.T) {
	f := readTS(t)
	hall, err := borgrec.NewHall(f.HallDraw.Name, bytes.NewReader(unhexT(t, f.HallDraw.Random)))
	if err != nil || hall != f.HallDraw.Hall {
		t.Fatalf("hall draw: %q %v, want %q", hall, err, f.HallDraw.Hall)
	}
	for name, h := range f.Records {
		b := unhexT(t, h)
		var re []byte
		switch {
		case name == "org0" || name == "org1":
			r, err := borgrec.DecodeOrg(b)
			re, err = enc(r, err)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case name == "group0":
			r, err := borgrec.DecodeGroup(b)
			if re, err = enc(r, err); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case name == "wrap":
			r, err := borgrec.DecodeWrap(b)
			if re, err = enc(r, err); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		default:
			r, err := borgrec.DecodeGrant(b)
			if re, err = enc(r, err); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if !bytes.Equal(re, b) {
			t.Errorf("%s does not re-encode to itself", name)
		}
	}
}

// TestTSGrantsAndWrap verifies the TypeScript-issued certificates, opens
// them for their subjects, and opens the wrap for each member.
func TestTSGrantsAndWrap(t *testing.T) {
	f := readTS(t)
	ctx := context.Background()
	for name, p := range map[string]Party{"alice": Alice, "bob": Bob} {
		g, err := borgrec.DecodeGrant(unhexT(t, f.Records["grant"+map[string]string{"alice": "Alice", "bob": "Bob"}[name]]))
		if err != nil {
			t.Fatal(err)
		}
		if err := g.CheckCertificate(ctx); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		open, err := g.OpenFields(ctx, p.Wallet())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for k, v := range f.GrantValues[name] {
			if open[k] != v {
				t.Errorf("%s %s: %q, want %q", name, k, open[k], v)
			}
		}
		rv, err := g.CheckReveal()
		if err != nil || (name == "alice") != (rv != nil) || (rv != nil && rv["role"] != f.GrantValues[name]["role"]) {
			t.Errorf("%s reveal: %v %v", name, rv, err)
		}
	}
	grec := unhexT(t, f.Records["group0"])
	w, err := borgrec.DecodeWrap(unhexT(t, f.Records["wrap"]))
	if err != nil {
		t.Fatal(err)
	}
	g, err := borgrec.CheckEpochToken(grec, w)
	if err != nil {
		t.Fatal(err)
	}
	org, _ := ec.PublicKeyFromBytes(w.Org)
	for _, p := range []Party{Alice, Bob} {
		var got [32]byte
		var ok bool
		for _, e := range w.Entries {
			if k, err := borgrec.Unwrap(ctx, p.Wallet(), e, org, w.EpochID, uint32(w.Epoch), g.Commitment); err == nil {
				got, ok = k, true
			}
		}
		if !ok || hex.EncodeToString(got[:]) != f.EpochKey {
			t.Errorf("%s does not open the TypeScript wrap", p.Name)
		}
	}
	o0, _ := borgrec.DecodeOrg(unhexT(t, f.Records["org0"]))
	o1, _ := borgrec.DecodeOrg(unhexT(t, f.Records["org1"]))
	if err := borgrec.CheckOrgTransition(o0, o1.Prev, o1); err != nil {
		t.Error(err)
	}
}

// TestTSTransactions admits every transaction the TypeScript side built, and
// rebuilds each token's BEEF from its two transactions byte for byte.
func TestTSTransactions(t *testing.T) {
	f := readTS(t)
	h := Headers{}
	for _, x := range f.Headers {
		r, err := chainhash.NewHashFromHex(x.MerkleRoot)
		if err != nil {
			t.Fatal(err)
		}
		h[x.Height] = *r
	}
	for _, c := range f.Transactions {
		raw := unhexT(t, c.BEEF)
		a, err := borgrec.Admit(context.Background(), raw, nil, borgrec.Host{Hall: f.Hall, Headers: h})
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if a.Kind.String() != c.Kind || chainhash.Hash(a.Txid).String() != c.Txid {
			t.Errorf("%s: admitted as %s %x", c.Name, a.Kind, a.Txid)
		}
		if a.Token == nil {
			continue
		}
		b, err := transaction.NewBeefFromBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		if c.Name == "org-1-same-block" && len(b.BUMPs) != 1 {
			t.Errorf("%s: %d BUMPs, want the one merged path", c.Name, len(b.BUMPs))
		}
		tok := b.FindTransaction(c.Txid)
		par := b.FindTransaction(chainhash.Hash(a.Parent).String())
		rebuilt, err := borgrec.TokenBEEF(tok, par)
		if err != nil || !bytes.Equal(rebuilt, raw) {
			t.Errorf("%s: the Go TokenBEEF differs from the TypeScript one (%v)", c.Name, err)
		}
	}
}
