package vectors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"

	"github.com/lightwebinc/borg/internal/borgrec"
)

type txFile struct {
	Hall           string `json:"hall"`
	MaxBEEF        int    `json:"maxBeef"`
	MaxSweepInputs int    `json:"maxSweepInputs"`
	Headers        []struct {
		Height     uint32 `json:"height"`
		MerkleRoot string `json:"merkleRoot"`
	} `json:"headers"`
	Cases  []txJSON `json:"cases"`
	Stored []struct {
		Name     string `json:"name"`
		Txid     string `json:"txid"`
		Stored   string `json:"stored"`
		Seq      uint64 `json:"seq"`
		Kind     string `json:"kind"`
		Record   string `json:"record"`
		Parent   string `json:"parentTxid"`
		Replayed string `json:"replayBeef"`
	} `json:"stored"`
}

func readTxFile(t *testing.T) (*txFile, Headers) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "transaction-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f txFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	h := Headers{}
	for _, x := range f.Headers {
		r, err := chainhash.NewHashFromHex(x.MerkleRoot)
		if err != nil {
			t.Fatal(err)
		}
		h[x.Height] = *r
	}
	return &f, h
}

// TestTransactionFile replays transaction-v1.json from the file alone, as a
// second implementation would: every case admitted or refused with its
// reason, and every admission's instructions and records as written.
func TestTransactionFile(t *testing.T) {
	f, h := readTxFile(t)
	if len(f.Cases) < 90 {
		t.Fatalf("%d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		beef, _ := hex.DecodeString(c.BEEF)
		held := []int{}
		for _, coin := range c.Coins {
			held = append(held, coin.InputIndex)
		}
		a, err := borgrec.Admit(context.Background(), beef, held, borgrec.Host{Hall: f.Hall, Headers: h, MaxBEEF: f.MaxBEEF, MaxSweepInputs: c.MaxSweepInputs})
		if c.Verdict == "refuse" {
			if got := borgrec.Reason(err); got != c.Reason {
				t.Errorf("%s: %q (%v), want %q", c.Name, got, err, c.Reason)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused: %v", c.Name, err)
			continue
		}
		got, _ := json.Marshal(admission(a))
		want, _ := json.Marshal(c.Admits)
		if string(got) != string(want) {
			t.Errorf("%s: admits %s, want %s", c.Name, got, want)
		}
	}
}

// TestStoredTokens reads each stored token without its parent and
// reassembles the BEEF it was admitted in.
func TestStoredTokens(t *testing.T) {
	f, _ := readTxFile(t)
	byTxid := map[string][]byte{}
	for _, s := range f.Stored {
		b, _ := hex.DecodeString(s.Stored)
		byTxid[s.Txid] = b
	}
	for _, s := range f.Stored {
		b, _ := hex.DecodeString(s.Stored)
		tx, tok, err := borgrec.StoredToken(b)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		if tok.Seq() != s.Seq || string(tok.Kind) != s.Kind || hex.EncodeToString(tok.Record) != s.Record {
			t.Errorf("%s: read %d %s", s.Name, tok.Seq(), tok.Kind)
		}
		pb, ok := byTxid[s.Parent]
		if !ok {
			continue // a chain tree, which no host keeps
		}
		ptx, _, err := borgrec.StoredToken(pb)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := borgrec.TokenBEEF(tx, ptx)
		if err != nil || hex.EncodeToString(replay) != s.Replayed {
			t.Errorf("%s: replay differs (%v)", s.Name, err)
		}
	}
}

// TestIndexFile folds index-v1.json's chains and rows from the file alone.
func TestIndexFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(dir, "index-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Broken []struct {
			Label    string   `json:"label"`
			Kind     string   `json:"kind"`
			Records  []string `json:"records"`
			Statuses []string `json:"statuses"`
		} `json:"broken"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Broken {
		of, gf := borgrec.NewOrgFold(), borgrec.NewGroupFold()
		for i, r := range c.Records {
			raw, _ := hex.DecodeString(r)
			var st string
			if c.Kind == "org" {
				o, err := borgrec.DecodeOrg(raw)
				if err != nil {
					t.Fatal(err)
				}
				st = of.Apply(o)
			} else {
				g, err := borgrec.DecodeGroup(raw)
				if err != nil {
					t.Fatal(err)
				}
				st = gf.Apply(g)
			}
			if st != c.Statuses[i] {
				t.Errorf("%s: token %d %s, want %s", c.Label, i, st, c.Statuses[i])
			}
		}
	}
}

// TestAdmitSurvivesGarbage feeds truncations and bit flips of every case's
// BEEF to Admit: it refuses or admits, and never panics.
func TestAdmitSurvivesGarbage(t *testing.T) {
	f, h := readTxFile(t)
	host := borgrec.Host{Hall: f.Hall, Headers: h}
	for _, c := range f.Cases {
		beef, _ := hex.DecodeString(c.BEEF)
		for _, cut := range []int{0, 1, 4, 36, len(beef) / 2, len(beef) - 1} {
			_, _ = borgrec.Admit(context.Background(), beef[:cut], nil, host)
		}
		for i := 0; i < len(beef); i += 1 + len(beef)/64 {
			x := append([]byte(nil), beef...)
			x[i] ^= 0x5a
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s: flip at %d panics: %v", c.Name, i, r)
					}
				}()
				_, _ = borgrec.Admit(context.Background(), x, nil, host)
			}()
		}
	}
	_ = fmt.Sprint
}
