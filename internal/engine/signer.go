package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/state"
)

// The signer key (spec section 7.4) is how a second instance of the
// organisation's tooling, or a home rebuilt on a new machine, comes to hold
// an epoch key another instance sampled: the way any member does. It is a
// key derived from the organisation's identity key, so every instance
// derives it; it holds a membership grant and is a member of each group
// like any other key, so each epoch's key is wrapped to it; and Sync opens
// those wraps. Nothing about it is special to a host or a reader.

// SignerRole is the role of the signer key's grant: a label.
const SignerRole = "signer"

// SignerKey is the organisation's signer key, compressed.
func (e *Engine) SignerKey() ([]byte, error) { return borgrec.SignerKey(e.OrgKey()) }

// signerWallet is a wallet over the signer key's private key, which opens
// the wraps addressed to it.
func (e *Engine) signerWallet() (wallet.Interface, error) {
	if e.Key == nil {
		return nil, errors.New("the organisation's identity key is not open: the signer key cannot be derived")
	}
	k, err := borgrec.SignerPrivate(e.Key)
	if err != nil {
		return nil, err
	}
	return wallet.NewCompletedProtoWallet(k)
}

// signerActive reports whether the signer key is kept a member of every
// group this home creates or changes: when more than one instance can sign
// (Opts.Signers), or once the organisation has granted it (borg org
// signer), whatever the setting.
func (e *Engine) signerActive() bool {
	if e.Opts.Signers > 1 {
		return true
	}
	o := e.St.Org
	sk, err := e.SignerKey()
	if o == nil || err != nil {
		return false
	}
	h := hex.EncodeToString(sk)
	for i := range o.Grants {
		if g := &o.Grants[i]; g.Subject == h && g.Admitted && !g.Revoked && g.Swept == "" {
			return true
		}
	}
	return false
}

// signerReq is the grant the signer key is issued.
func (e *Engine) signerReq() (GrantReq, error) {
	sk, err := e.SignerKey()
	if err != nil {
		return GrantReq{}, err
	}
	pub, err := ec.PublicKeyFromBytes(sk)
	if err != nil {
		return GrantReq{}, err
	}
	return GrantReq{Subject: pub, Role: SignerRole, Scope: "member", Expiry: e.Now().Add(limits.SignerExpiry)}, nil
}

// EnsureSigner gives the signer key a current grant when it holds none (an
// organisation token admits it), and returns the key. A grant that expired
// is replaced before any group transition, so the signer key is never
// rotation debt.
func (e *Engine) EnsureSigner(ctx context.Context) ([]byte, error) {
	o, err := e.org()
	if err != nil {
		return nil, err
	}
	sk, err := e.SignerKey()
	if err != nil {
		return nil, err
	}
	h := hex.EncodeToString(sk)
	if e.currentGrant(h) != nil {
		return sk, nil
	}
	if e.Key == nil {
		return nil, errors.New("the organisation's identity key is not open: the signer key cannot be granted")
	}
	var g *state.Grant
	for i := range o.Grants {
		if og := &o.Grants[i]; og.Subject == h && !og.Admitted && !og.Revoked && og.Swept == "" {
			g = og
		}
	}
	if g == nil {
		req, err := e.signerReq()
		if err != nil {
			return nil, err
		}
		if g, err = e.IssueGrant(ctx, e.Key, req); err != nil {
			return nil, fmt.Errorf("the signer key's grant: %w", err)
		}
		e.note("grant %s issued to the organisation's signer key %s", Short(g.Txid), h)
	}
	if err := e.OrgTransition(ctx, []*state.Grant{o.GrantByTxid(g.Txid)}, nil, "organisation token (admit the signer key)"); err != nil {
		return nil, err
	}
	return sk, nil
}
