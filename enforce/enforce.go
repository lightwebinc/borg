// Package enforce is the whole of borg's decision procedure (docs/spec.md
// section 13) behind one call, for a service that decides on a requester's
// behalf: the organisation's latest token and the subject's grants, groups
// and policy records, each verified against the service's own headers,
// folded into a verdict, signed into a receipt and handed back. A service
// adds authorisation in one file and keeps its own interface in front of
// this one, so the dependency on borg lives here and nowhere else in it.
//
// It fails closed. Any read it cannot complete is a deny with a reason, and
// an allow is reused only within the freshness the service states, which
// the receipt makes visible by naming the seq it rested on.
package enforce

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/authz"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/reader"
)

// DefaultFreshness is how long an allow is reused for one subject and one
// permission at one seq when the service states no bound of its own.
const DefaultFreshness = 60 * time.Second

// MaxCached bounds the allows an Enforcer keeps; past it the cache is
// cleared, which costs a read and never an answer.
const MaxCached = 10000

// Enforcer decides for one organisation. Make one per organisation a
// service answers for and keep it: it holds the organisation's latest view
// and the allows it may reuse.
type Enforcer struct {
	// Reader asks the hosts and verifies what they answer. Its Headers are
	// the service's own, which is what makes a decision the service's.
	Reader *reader.Client
	// Spends says whether an outpoint is spent: a node or an index. Without
	// one nothing is current, and every decision denies (spec section 13).
	Spends reader.Spends
	// Wallet signs receipts; its identity key is the service's, and is the
	// key a checker verifies the receipt under. Originator is the wallet's
	// originator for the service.
	Wallet     wallet.Interface
	Originator string
	// Hall and Org name the organisation.
	Hall string
	Org  []byte
	// Freshness bounds how long an allow is reused; zero is
	// DefaultFreshness. A deny is never reused: a member just granted is
	// read afresh.
	Freshness time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu    sync.Mutex
	by    []byte
	view  *reader.OrgView
	seen  time.Time
	cache map[string]cached
}

type cached struct {
	verdict Verdict
	seq     uint64
	at      time.Time
}

// Verdict is one decision: the reader's, the signed receipt a checker can
// verify alone, and whether it was reused from the cache.
type Verdict struct {
	Allow    bool
	Reason   string
	Decision reader.Decision
	Receipt  *authz.Signed
	Cached   bool
}

// ErrNotReady is a nil reader, spend source or wallet: the Enforcer cannot
// decide, so it denies.
var ErrNotReady = errors.New("enforce: a reader, a spend source and a wallet are required")

func (e *Enforcer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Enforcer) freshness() time.Duration {
	if e.Freshness > 0 {
		return e.Freshness
	}
	return DefaultFreshness
}

// identity is the wallet's identity key, read once.
func (e *Enforcer) identity(ctx context.Context) ([]byte, error) {
	if e.by != nil {
		return e.by, nil
	}
	res, err := e.Wallet.GetPublicKey(ctx, wallet.GetPublicKeyArgs{IdentityKey: true}, e.Originator)
	if err != nil {
		return nil, fmt.Errorf("enforce: the wallet's identity key: %w", err)
	}
	e.by = res.PublicKey.Compressed()
	return e.by, nil
}

// orgView is the organisation's latest view, read again once it is older
// than the freshness: the seq a decision rests on is at most that stale.
func (e *Enforcer) orgView(ctx context.Context, now time.Time) (*reader.OrgView, error) {
	if e.view != nil && now.Sub(e.seen) < e.freshness() {
		return e.view, nil
	}
	ov, err := e.Reader.Org(ctx, e.Hall, e.Org)
	if err != nil {
		return nil, err
	}
	e.view, e.seen = ov, now
	e.cache = nil
	return ov, nil
}

// Allow decides whether subject holds permission in the organisation now.
// The permission is what a service asks, in full and with no wildcard. The
// verdict is a deny with a reason whenever the read could not be completed,
// and the error says why; a verdict with a receipt is one a checker can
// verify, whichever way it went.
func (e *Enforcer) Allow(ctx context.Context, subject []byte, permission string) (Verdict, error) {
	if e.Reader == nil || e.Spends == nil || e.Wallet == nil {
		return Verdict{Reason: "not-ready"}, ErrNotReady
	}
	if _, err := borgrec.Asked(permission); err != nil {
		return Verdict{Reason: "permission"}, fmt.Errorf("enforce: %q is not a permission a service asks: %w", permission, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	by, err := e.identity(ctx)
	if err != nil {
		return Verdict{Reason: "incomplete"}, err
	}
	ov, err := e.orgView(ctx, now)
	if err != nil {
		// No token, no seq, nothing to sign over: the deny is the error.
		return Verdict{Reason: "incomplete"}, fmt.Errorf("enforce: the organisation's chain: %w", err)
	}
	key := string(subject) + "|" + permission
	if c, ok := e.cache[key]; ok && c.seq == ov.Token.Seq && now.Sub(c.at) < e.freshness() {
		v := c.verdict
		v.Cached = true
		return v, nil
	}
	d, err := e.Reader.Allows(ctx, ov, subject, permission, e.Spends, now)
	if err != nil {
		return Verdict{Reason: "incomplete", Decision: d}, err
	}
	r := &authz.Receipt{
		By: by, Org: e.Org, Hall: e.Hall, Subject: subject, Permission: permission,
		Allow: d.Allow, Seq: d.Seq, Grants: d.Grants, Policies: d.Policies,
		Time: uint64(now.Unix()), Reason: d.Reason, //nolint:gosec // a clock after 1970
	}
	h, err := chainhash.NewHashFromHex(d.Token)
	if err != nil {
		return Verdict{Reason: "incomplete", Decision: d}, fmt.Errorf("enforce: the token's txid: %w", err)
	}
	r.Token = *h
	signed, err := authz.Sign(ctx, e.Wallet, e.Originator, r)
	if err != nil {
		return Verdict{Reason: "incomplete", Decision: d}, err
	}
	v := Verdict{Allow: d.Allow, Reason: d.Reason, Decision: d, Receipt: signed}
	if d.Allow {
		if len(e.cache) >= MaxCached {
			e.cache = nil
		}
		if e.cache == nil {
			e.cache = map[string]cached{}
		}
		e.cache[key] = cached{verdict: v, seq: d.Seq, at: now}
	}
	return v, nil
}

// Forget drops every reused allow, for a service that learns of a change
// before the freshness would.
func (e *Enforcer) Forget() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cache = nil
	e.view = nil
}
