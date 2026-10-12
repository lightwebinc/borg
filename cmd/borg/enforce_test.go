package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/borg/enforce"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/reader"
)

// enforcer is what a service holds: a reader over the lab's two hosts with
// the service's own headers, a spend source, and a wallet of its own to
// sign receipts with.
func (h *harness) enforcer(t *testing.T, org string) *enforce.Enforcer {
	t.Helper()
	w, err := bwallet.Create(t.TempDir(), engine.Profile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := nodeapi.ParseChain("asset:"+h.chainS.URL, nodeapi.ChainOptions{Client: http.DefaultClient, Headers: h.chain})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(org)
	if err != nil {
		t.Fatal(err)
	}
	return &enforce.Enforcer{
		Reader:    &reader.Client{Hosts: []string{h.aS.URL, h.bS.URL}, Headers: h.chain, HTTP: http.DefaultClient, Timeout: 5 * time.Second},
		Spends:    spendSource{c},
		Wallet:    w.Signer(),
		Hall:      h.hall,
		Org:       raw,
		Freshness: time.Minute,
	}
}

// A service decides in one call, gets a receipt it did not have to build,
// and anyone can check that receipt alone. An allow is reused within the
// freshness and a deny is not; a change on the chain is seen once the view
// is read again.
func TestEnforceDecidesAndSignsAReceipt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "role", "define", "releaser", "-permission", "bsecret/release/payroll")
	h.must("org", "bind", "releaser", "-to", o.group)
	e := h.enforcer(t, o.key)
	alice, _ := hex.DecodeString(o.alice)
	stranger, _ := hex.DecodeString(h.identity("stranger", false))
	ctx := context.Background()

	v, err := e.Allow(ctx, alice, "bsecret/release/payroll")
	if err != nil || !v.Allow || v.Receipt == nil {
		t.Fatalf("alice, in the bound group: allow=%v err=%v receipt=%v", v.Allow, err, v.Receipt != nil)
	}
	// The receipt verifies under the service's own key and names the state.
	r, err := v.Receipt.Verify()
	if err != nil {
		t.Fatalf("the receipt does not verify: %v", err)
	}
	if !r.Allow || r.Seq != v.Decision.Seq || hex.EncodeToString(r.Subject) != o.alice || r.Permission != "bsecret/release/payroll" || len(r.Policies) != 2 {
		t.Fatalf("the receipt does not say what was decided: %+v", r)
	}
	// Reused within the freshness.
	if v2, _ := e.Allow(ctx, alice, "bsecret/release/payroll"); !v2.Cached || !v2.Allow {
		t.Fatalf("an allow within the freshness is reused: cached=%v", v2.Cached)
	}
	// A deny carries a receipt too, and is never reused: a member holding
	// nothing that matches, and a key that is no member at all.
	d, err := e.Allow(ctx, alice, "bstore/put/photos/a.jpg")
	if err != nil || d.Allow || d.Receipt == nil || d.Reason != "no-match" {
		t.Fatalf("alice, a permission the role does not carry: allow=%v reason=%q err=%v", d.Allow, d.Reason, err)
	}
	if rd, err := d.Receipt.Verify(); err != nil || rd.Allow || rd.Reason != "no-match" {
		t.Fatalf("the deny's receipt: %+v %v", rd, err)
	}
	if d2, _ := e.Allow(ctx, alice, "bstore/put/photos/a.jpg"); d2.Cached {
		t.Fatal("a deny is not reused")
	}
	s, err := e.Allow(ctx, stranger, "bsecret/release/payroll")
	if err != nil || s.Allow || s.Receipt == nil || s.Reason != "not-member" {
		t.Fatalf("a stranger: allow=%v reason=%q err=%v", s.Allow, s.Reason, err)
	}
	// A wildcard is not something a service asks.
	if _, err := e.Allow(ctx, alice, "bsecret/release/*"); err == nil || !strings.Contains(err.Error(), "not a permission a service asks") {
		t.Fatalf("a wildcard ask: %v", err)
	}
	// The role retired on the chain: the cached allow stands only until the
	// view is read again, which Forget does at once.
	h.must("org", "role", "retire", "releaser")
	if v3, _ := e.Allow(ctx, alice, "bsecret/release/payroll"); !v3.Cached || !v3.Allow {
		t.Fatal("within the freshness the earlier allow is what the service stated it would reuse")
	}
	e.Forget()
	v4, err := e.Allow(ctx, alice, "bsecret/release/payroll")
	if err != nil || v4.Allow || v4.Cached {
		t.Fatalf("after the retire, read afresh: allow=%v cached=%v err=%v", v4.Allow, v4.Cached, err)
	}
	if r4, err := v4.Receipt.Verify(); err != nil || r4.Seq <= r.Seq {
		t.Fatalf("the new receipt names a later seq: %d after %d (%v)", r4.Seq, r.Seq, err)
	}
}

// Without a spend source nothing is current, and the enforcer denies before
// it asks anything: fail closed is the default, not a configuration.
func TestEnforceDeniesWithoutASpendSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	e := h.enforcer(t, o.key)
	e.Spends = nil
	alice, _ := hex.DecodeString(o.alice)
	v, err := e.Allow(context.Background(), alice, "bsecret/release/payroll")
	if err == nil || v.Allow || v.Reason != "not-ready" {
		t.Fatalf("allow=%v reason=%q err=%v", v.Allow, v.Reason, err)
	}
}
