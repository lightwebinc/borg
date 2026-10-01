package engine

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/borg/internal/borgrec"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/internal/testchain"
)

const hall = "example_abcdefghij"

func key(t *testing.T) []byte {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.PubKey().Compressed()
}

// offline is an engine whose legs are never reached: every check here
// refuses before anything is built or sent.
func offline(t *testing.T) (*Engine, *state.State) {
	t.Helper()
	dir := t.TempDir()
	w, err := bwallet.Create(dir, Profile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(dir, w.Signer().IdentityHex())
	if err != nil {
		t.Fatal(err)
	}
	legs := Legs{Settler: &publish.TCPIngress{Addr: "192.0.2.1:1"}, Asset: &nodeapi.Asset{Base: "http://192.0.2.1"},
		Facade: &publish.Facade{Base: "http://192.0.2.1"}, Headers: testchain.New(1)}
	e, err := New(st, w.Signer(), w.Pool, legs, Options{TreeCount: 4, ChainTreeCount: 2, TreeSats: 1, Entries: 8})
	if err != nil {
		t.Fatal(err)
	}
	return e, st
}

// withGroup gives the state an organisation with a group of members at
// seq 0, each holding a grant that ends at expiry.
func withGroup(t *testing.T, e *Engine, st *state.State, members [][]byte, expiry time.Time) string {
	t.Helper()
	sortKeys(members)
	o := &state.Org{Hall: hall, Name: "x", Tokens: []state.Token{{Seq: 0, Txid: strings.Repeat("ab", 32)}}}
	for _, m := range members {
		o.Grants = append(o.Grants, state.Grant{Subject: hex.EncodeToString(m), Admitted: true, Scope: "member",
			Expiry: expiry.UTC().Format(borgrec.ExpiryLayout)})
	}
	rec := &borgrec.Group{Hall: hall, Org: e.OrgKey(), Group: [16]byte{7}, Name: "g", Commitment: [32]byte{1},
		Add: members, Count: uint64(len(members)), Members: borgrec.MembersRoot(members), Created: 1}
	b, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(rec.Group[:])
	o.Groups = []state.Group{{ID: id, Name: "g", Tokens: []state.Token{{Seq: 0, Txid: strings.Repeat("cd", 32), Record: hex.EncodeToString(b)}}}}
	st.Org = o
	return id
}

func TestDebtIsMembersWithoutACurrentGrant(t *testing.T) {
	e, st := offline(t)
	a, b := key(t), key(t)
	id := withGroup(t, e, st, [][]byte{a, b}, time.Now().Add(time.Hour))
	if d, err := e.Debt(st.Org.Group(id)); err != nil || len(d) != 0 {
		t.Fatalf("no debt: %v %d", err, len(d))
	}
	for i := range st.Org.Grants {
		if st.Org.Grants[i].Subject == hex.EncodeToString(b) {
			st.Org.Grants[i].Revoked = true
		}
	}
	d, err := e.Debt(st.Org.Group(id))
	if err != nil || len(d) != 1 || string(d[0]) != string(b) {
		t.Fatalf("a revoked grant is debt: %v %d", err, len(d))
	}
	e.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if d, _ := e.Debt(st.Org.Group(id)); len(d) != 2 {
		t.Fatalf("an expired grant is debt: %d", len(d))
	}
}

func TestGroupChangeRefusesBeforeBuilding(t *testing.T) {
	e, st := offline(t)
	a, b, stranger := key(t), key(t), key(t)
	id := withGroup(t, e, st, [][]byte{a, b}, time.Now().Add(time.Hour))
	ctx := context.Background()
	for _, c := range []struct {
		name string
		ch   Change
		want string
	}{
		{"remove a non-member", Change{Remove: [][]byte{stranger}}, "is not a member"},
		{"add a member", Change{Add: [][]byte{a}}, "is a member of group"},
		{"add a key with no grant", Change{Add: [][]byte{stranger}}, "holds no current grant"},
		{"add the organisation", Change{Add: [][]byte{e.OrgKey()}}, "neither the organisation nor the anyone key"},
		{"add the anyone key", Change{Add: [][]byte{borgrec.AnyoneKey()}}, "neither the organisation nor the anyone key"},
		{"nothing", Change{}, "changes nothing"},
	} {
		if _, err := e.GroupChange(ctx, id, c.ch); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := e.GroupChange(ctx, strings.Repeat("00", 16), Change{Rotate: true}); err == nil {
		t.Error("an unknown group")
	}
	if _, err := e.CreateGroup(ctx, "h", [][]byte{stranger}, false, 0); err == nil || !strings.Contains(err.Error(), "holds no current grant") {
		t.Errorf("create with a key with no grant: %v", err)
	}
	if _, err := e.CreateGroup(ctx, "h", [][]byte{a, a}, false, 0); err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Errorf("create with a key twice: %v", err)
	}
}

func TestLostErrorSaysTheKeyIsDiscarded(t *testing.T) {
	err := &LostError{What: "group x", Txid: strings.Repeat("a", 64), By: strings.Repeat("b", 64)}
	if !strings.Contains(err.Error(), "epoch key it sampled is discarded") || !strings.Contains(err.Error(), "bbbbbbbbbbbb") {
		t.Fatal(err)
	}
}
