package enforce

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/reader"
)

// noHeaders validates nothing; the reads under test never get as far.
type noHeaders struct{}

func (noHeaders) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	return false, nil
}
func (noHeaders) CurrentHeight(context.Context) (uint32, error) { return 1, nil }

type nilSpends struct{}

func (nilSpends) Spender(context.Context, string, uint32) (string, error) { return "", nil }

// A Decider over hosts that are down fails closed, asks a failed host once
// a DenyFor, keeps no failure of a read the caller cut short, keeps an
// allow through the outage within StaleFor and not past it, reads the
// token again at DenyFor for a denied subject, and denies without a spend
// source or for a wildcard.
func TestDeciderOutage(t *testing.T) {
	asks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asks++
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	clock := time.Unix(5000, 0)
	d := &Decider{Reader: &reader.Client{Hosts: []string{srv.URL}, Headers: noHeaders{}, Timeout: time.Second, Floor: &reader.MemFloor{}},
		Spends: nilSpends{}, AllowFor: 30 * time.Second, DenyFor: 10 * time.Second, StaleFor: 600 * time.Second,
		Now: func() time.Time { return clock }}
	org := append([]byte{0x02}, make([]byte, 32)...)
	const hall, perm = "example_hall_vkqzmtrwxp", "bdns/write/example.com/www.example.com"
	vk := hall + "|" + fmt.Sprintf("%x", org)
	ctx := context.Background()

	cut, cancel := context.WithCancel(ctx)
	cancel()
	if ok, err := d.Allows(cut, hall, org, []byte{0x02, 9}, perm); ok || err == nil || len(d.failed) != 0 {
		t.Fatalf("cancelled: %v %v, %d kept", ok, err, len(d.failed))
	}
	ok, err := d.Allows(ctx, hall, org, []byte{0x02, 9}, perm)
	if ok || err == nil || errors.Is(err, ErrStale) || asks == 0 {
		t.Fatalf("down, never read: %v %v, %d asks", ok, err, asks)
	}
	n := asks
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 8}, perm); ok || err == nil || asks != n {
		t.Fatalf("within DenyFor a failure is not read again: %v %v, %d asks", ok, err, asks-n)
	}
	d.answers = setIn(d.answers, vk+"|0207|"+perm, deciderAnswer{allow: true, at: clock})
	clock = clock.Add(11 * time.Second)
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 7}, perm); !ok || !errors.Is(err, ErrStale) || asks == n {
		t.Fatalf("stale allow: %v %v", ok, err)
	}
	clock = clock.Add(600 * time.Second)
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 7}, perm); ok || err == nil {
		t.Fatalf("past StaleFor: %v %v", ok, err)
	}

	// A view read 11 s ago: kept for a subject allowed, read again for one
	// denied, so a subject just granted waits DenyFor.
	d.views = setIn(d.views, vk, deciderView{view: &reader.OrgView{ChainView: &reader.ChainView{Hall: hall, Org: org}, Token: &borgrec.Org{Seq: 4}}, at: clock.Add(-11 * time.Second)})
	d.failed = nil
	d.answers = setIn(d.answers, vk+"|0206|"+perm, deciderAnswer{allow: true, seq: 4, at: clock.Add(-11 * time.Second)})
	d.answers = setIn(d.answers, vk+"|0205|"+perm, deciderAnswer{allow: false, seq: 4, at: clock.Add(-11 * time.Second)})
	n = asks
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 6}, perm); !ok || err != nil || asks != n {
		t.Fatalf("allow within AllowFor: %v %v, %d asks", ok, err, asks-n)
	}
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 5}, perm); ok || err == nil || asks == n {
		t.Fatalf("a deny past DenyFor reads the token again: %v %v, %d asks", ok, err, asks-n)
	}

	// No host answers the grants: a failed read, not a deny. An allow of
	// the same seq stands; one of an earlier seq, once a later token is
	// verified, never does; with StaleFor zero none does.
	d.views = setIn(d.views, vk, deciderView{view: &reader.OrgView{ChainView: &reader.ChainView{Hall: hall, Org: org}, Token: &borgrec.Org{Seq: 4}}, at: clock})
	d.answers = setIn(d.answers, vk+"|0204|"+perm, deciderAnswer{allow: true, seq: 4, at: clock.Add(-31 * time.Second)})
	d.answers = setIn(d.answers, vk+"|0203|"+perm, deciderAnswer{allow: true, seq: 3, at: clock.Add(-31 * time.Second)})
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 4}, perm); !ok || !errors.Is(err, ErrStale) {
		t.Fatalf("grants unanswered, same seq: %v %v", ok, err)
	}
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 3}, perm); ok || err == nil || errors.Is(err, ErrStale) {
		t.Fatalf("an allow of an earlier seq: %v %v", ok, err)
	}
	d.StaleFor = 0
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 4}, perm); ok || err == nil {
		t.Fatalf("StaleFor zero: %v %v", ok, err)
	}
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 6}, "bdns/write/*"); ok || err == nil {
		t.Fatalf("a wildcard asked: %v %v", ok, err)
	}
	d.Spends = nil
	if ok, err = d.Allows(ctx, hall, org, []byte{0x02, 6}, perm); ok || err == nil {
		t.Fatalf("no spends: %v %v", ok, err)
	}
}

// MemFloor never lowers a seq.
func TestMemFloor(t *testing.T) {
	var f reader.MemFloor
	if _, _, ok := f.Seen("h", "o", "org"); ok {
		t.Fatal("an empty floor remembers")
	}
	f.See("h", "o", "org", 5, "a")
	f.See("h", "o", "org", 3, "b")
	if seq, txid, ok := f.Seen("h", "o", "org"); !ok || seq != 5 || txid != "a" {
		t.Fatalf("%d %s %v", seq, txid, ok)
	}
	f.See("h", "o", "org", 6, "c")
	if seq, txid, _ := f.Seen("h", "o", "org"); seq != 6 || txid != "c" {
		t.Fatalf("%d %s", seq, txid)
	}
}

type errSpends struct{ by string }

func (e errSpends) Spender(context.Context, string, uint32) (string, error) {
	if e.by == "" {
		return "", fmt.Errorf("node said %q", "SOMEWHERE-THE-NODE-NAMED-LONG")
	}
	return e.by, nil
}

// QuietSpends lowers the spender and leaves out what a node named.
func TestQuietSpends(t *testing.T) {
	if by, err := reader.QuietSpends(errSpends{by: "ABCDEF"}).Spender(context.Background(), "x", 0); err != nil || by != "abcdef" {
		t.Fatalf("%q %v", by, err)
	}
	if _, err := reader.QuietSpends(errSpends{}).Spender(context.Background(), "x", 0); err == nil || strings.Contains(err.Error(), "SOMEWHERE") {
		t.Fatalf("not quieted: %v", err)
	}
}
