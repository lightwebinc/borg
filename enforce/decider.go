package enforce

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/reader"
)

// Decider is the decision procedure of spec section 13 for a service that
// decides for itself, keeps no receipt and answers for any organization a
// caller names: a renderer that admits writers by a permission, say. It is
// Enforcer without the wallet and the receipt, and with what a service
// that asks often and from many places needs: denies reused too, a failed
// read remembered, and an allow kept through an outage for a bound the
// service states. Make one and keep it; it is safe for concurrent use and
// holds no lock over a read.
//
// It fails closed. A read it cannot complete is an error, and the answer is
// a deny, except an allow read before the failure and within StaleFor,
// which is answered true with an error that wraps ErrStale.
type Decider struct {
	// Reader asks the hosts and verifies what they answer against the
	// service's own headers. Give it a Floor (reader.MemFloor at least), so
	// a host set that answers an older organization token than one already
	// verified is refused; without one a Decider still never goes back to
	// an earlier seq itself, but a restart, or a second Decider, can.
	Reader *reader.Client
	// Spends says whether an outpoint is spent. Without one nothing is
	// current and every call denies.
	Spends reader.Spends
	// AllowFor is how long an allow is reused, and how often the
	// organization's latest token is read again while every subject asked
	// about is allowed; zero is DefaultFreshness. A revoked grant is
	// answered at most this long past the hosts holding the token that
	// revoked it.
	AllowFor time.Duration
	// DenyFor is how long a deny, or a failed read of an organization, is
	// reused, and how often the latest token is read again while a subject
	// is denied: so a subject just granted waits this long once the hosts
	// hold the token. Zero is DefaultDenyFor; it is held to AllowFor.
	DenyFor time.Duration
	// StaleFor is how long an allow stands through failed reads, from the
	// read that allowed it, and only while no later token is verified.
	// Zero keeps none: a failed read denies.
	StaleFor time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	views   map[string]deciderView
	answers map[string]deciderAnswer
	failed  map[string]deciderFailure
}

// DefaultDenyFor is how long a Decider reuses a deny when it is told no
// bound of its own.
const DefaultDenyFor = 10 * time.Second

// ErrStale marks an allow kept through a failed read: the organization
// could not be read, and the subject was allowed when it last was, within
// StaleFor.
var ErrStale = errors.New("enforce: the organization could not be read; its last allow stands")

type deciderView struct {
	view *reader.OrgView
	at   time.Time
}

type deciderAnswer struct {
	allow bool
	seq   uint64
	at    time.Time
}

type deciderFailure struct {
	err error
	at  time.Time
}

func (d *Decider) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Decider) allowFor() time.Duration {
	if d.AllowFor > 0 {
		return d.AllowFor
	}
	return DefaultFreshness
}

func (d *Decider) denyFor() time.Duration {
	if d.DenyFor > 0 {
		return min(d.DenyFor, d.allowFor())
	}
	return min(DefaultDenyFor, d.allowFor())
}

// Allows decides whether subject holds permission in the organization org
// of hall now. The permission is asked in full, with no wildcard.
func (d *Decider) Allows(ctx context.Context, hall string, org, subject []byte, permission string) (bool, error) {
	if d.Reader == nil || d.Spends == nil {
		return false, errors.New("enforce: a reader and a spend source are required")
	}
	if _, err := borgrec.Asked(permission); err != nil {
		return false, fmt.Errorf("enforce: %q is not a permission a service asks: %w", permission, err)
	}
	now := d.now()
	vk := hall + "|" + hex.EncodeToString(org)
	ak := vk + "|" + hex.EncodeToString(subject) + "|" + permission
	d.mu.Lock()
	v, f := d.views[vk], d.failed[vk]
	a, seen := d.answers[ak]
	d.mu.Unlock()
	// fail is a read that failed. An allow read within StaleFor stands,
	// but only at the seq it was read at: once a later token is verified,
	// an allow of an earlier one is never answered, so a host set that
	// withholds a revoking policy set cannot keep it.
	fail := func(err error) (bool, error) {
		if d.StaleFor > 0 && seen && a.allow && now.Sub(a.at) <= d.StaleFor && (v.view == nil || a.seq == v.view.Token.Seq) {
			return true, fmt.Errorf("%w: %v", ErrStale, err)
		}
		return false, err
	}
	stale := d.allowFor()
	if !seen || !a.allow {
		stale = d.denyFor()
	}
	if v.view == nil || now.Sub(v.at) >= stale {
		if f.err != nil && now.Sub(f.at) < d.denyFor() {
			return fail(f.err)
		}
		ov, err := d.Reader.Org(ctx, hall, org)
		if err != nil {
			err = fmt.Errorf("enforce: the organization's chain: %w", err)
			// A read cut short by the caller's own deadline says nothing of
			// the hosts, so it is not kept for other callers.
			if ctx.Err() == nil {
				d.mu.Lock()
				if len(d.failed) >= MaxCached {
					d.failed = nil
				}
				d.failed = setIn(d.failed, vk, deciderFailure{err: err, at: now})
				d.mu.Unlock()
			}
			return fail(err)
		}
		d.mu.Lock()
		// A read that lands after a later one keeps the later view: a
		// view never goes back to an earlier seq.
		if cur, ok := d.views[vk]; ok && cur.view.Token.Seq > ov.Token.Seq {
			v = cur
		} else {
			v = deciderView{view: ov, at: now}
			if len(d.views) >= MaxCached {
				d.views = nil
			}
			d.views = setIn(d.views, vk, v)
		}
		delete(d.failed, vk)
		d.mu.Unlock()
	}
	if seen && a.seq == v.view.Token.Seq && (a.allow && now.Sub(a.at) < d.allowFor() || !a.allow && now.Sub(a.at) < d.denyFor()) {
		return a.allow, nil
	}
	dec, err := d.Reader.Allows(ctx, v.view, subject, permission, d.Spends, now)
	if err != nil {
		return fail(err)
	}
	if dec.Reason == "incomplete" {
		// The policy set the token commits to could not be read whole: a
		// failed read, not a deny, and not kept as an answer.
		return fail(errors.New("enforce: the organization's policy set could not be read whole"))
	}
	if !dec.Allow && (ctx.Err() != nil || len(dec.HostErr) >= len(d.Reader.Hosts) && len(dec.Grants) == 0) {
		// No host answered the subject's grants (or the caller's context
		// ended the read): the reader folds that into "not-member" with
		// no error, and it is a failed read, not a deny, and not kept.
		return fail(fmt.Errorf("enforce: no host answered the subject's grants (%d host errors)", len(dec.HostErr)))
	}
	d.mu.Lock()
	if len(d.answers) >= MaxCached {
		d.answers = nil
	}
	d.answers = setIn(d.answers, ak, deciderAnswer{allow: dec.Allow, seq: dec.Seq, at: now})
	d.mu.Unlock()
	return dec.Allow, nil
}

// Forget drops every reused answer, view and failure.
func (d *Decider) Forget() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.views, d.answers, d.failed = nil, nil, nil
}

func setIn[V any](m map[string]V, k string, v V) map[string]V {
	if m == nil {
		m = map[string]V{}
	}
	m[k] = v
	return m
}
