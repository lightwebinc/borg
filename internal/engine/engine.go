// Package engine is a borg publisher: the organisation's tooling (docs/spec.md
// sections 7, 9 and 12), run from one identity's home. It issues grants,
// mints the organisation's and its groups' chain tokens, samples and wraps
// epoch keys, revokes by roll and by sweep, and publishes every object on
// the plane or to each host.
//
// It is built on bcommon's producer package. A Payer pays for funding trees
// and chain tokens from the home's coin pool, and Trees mints the three
// kinds of funding tree the contract keeps apart (spec section 9.4): grant
// trees, record trees (which fund wraps) and chain trees (which pay for a
// chain's first token), each under its own derived key. Grant and record
// trees are minted ahead, so the next is on its way before the current runs
// out. A funding tree is never published to a hall: every carrier carries
// its own, and a chain's first token carries its chain tree.
//
// Every object is persisted in the home's state before it is sent: a
// carrier with the funding output it spends marked used, a transition with
// the epoch key it commits to. A run that stops part way sends the same
// bytes again (Resume), and never a second carrier on one funding output or
// a second key for one epoch. A transition takes effect only when a
// transaction spending its predecessor is mined in the publisher's own
// headers and carries byte for byte the record built (spec section 7.3).
package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/internal/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/reader"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/internal/unicast"
)

// Profile is the embedded wallet's profile: the fund key under [1, "borg
// organisation"] key id fund, and the registered basket.
var Profile = bwallet.Profile{
	FundProtocol:   borgrec.Protocol,
	FundKeyID:      borgrec.KeyFund,
	FundBasket:     "borg fund",
	Version:        "borg-1",
	LegacyPoolFile: "borg-pool.json",
}

// derivationOf is the derivation a tree kind's outputs are locked under.
func derivationOf(kind string) pushdrop.Derivation {
	switch kind {
	case state.TreeGrant:
		return borgrec.GrantDerivation
	case state.TreeRecord:
		return borgrec.RecordDerivation
	}
	return borgrec.ChainDerivation
}

// Params are the carrier parameters of a tree kind: its derivation and the
// funding tag. A payload is checked by the host's rules before anything is
// persisted, so it is not checked again here.
func Params(kind string) carrier.Params {
	return carrier.Params{Derivation: derivationOf(kind), FundingTag: borgrec.TagFunding,
		ValidatePayload: func([]byte) error { return nil }, ErrIdentity: borgrec.ErrIdentity}
}

// Options are a publisher's choices. None of them is frozen.
type Options struct {
	// TreeCount sizes grant and record trees, ChainTreeCount chain trees;
	// TreeSats is each output's value; Ahead is how few outputs a tree has
	// left when the next is minted ahead.
	TreeCount      int
	ChainTreeCount int
	TreeSats       uint64
	Ahead          uint32
	// Fees is the fee policy of trees, tokens, sweeps and payments.
	Fees mint.Fees
	// ObjectBound is the plane's object bound, in bytes.
	ObjectBound int
	// Poll paces proof waits and Wait bounds one.
	Poll time.Duration
	Wait time.Duration
	// Delay is the propagation delay before a mined object is published.
	Delay time.Duration
	// Signers is how many tooling instances can sign transitions: above
	// one, wraps wait one further block.
	Signers int
	// Entries is the most wrap entries one wrap record carries.
	Entries int
}

// Legs are the endpoints a publisher talks to. Objects go to exactly one of
// Facade (mode plane) and Hosts (mode unicast). In mode plane, Direct are
// the other hosts every mined object is also submitted to. Reader confirms,
// by lookup, a host that admitted nothing. Headers are the publisher's own
// block headers: a mined object is published only once its block is in
// them.
type Legs struct {
	Settler publish.Settler
	Asset   *nodeapi.Asset
	Arcade  *publish.Arcade
	Facade  *publish.Facade
	Hosts   *unicast.Set
	Direct  *unicast.Set
	Reader  *reader.Client
	Headers chaintracker.ChainTracker
}

// Engine is one publisher run over one home.
type Engine struct {
	St     *state.State
	Signer *bwallet.Signer
	Pool   *bwallet.Pool
	Legs   Legs
	Opts   Options
	// Now is the clock records are stamped with; Sleep waits.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Note receives progress lines.
	Note    func(format string, args ...any)
	Verbose bool

	payer   *producer.Payer
	kept    *producer.Kept
	trees   map[string]*producer.Trees
	pending *pendingTx
}

// New builds an engine over a home's state.
func New(st *state.State, signer *bwallet.Signer, pool *bwallet.Pool, legs Legs, o Options) (*Engine, error) {
	if st.Identity != signer.IdentityHex() {
		return nil, fmt.Errorf("the state belongs to identity %s, not this home's %s", st.Identity, signer.IdentityHex())
	}
	if legs.Settler == nil || legs.Asset == nil {
		return nil, errors.New("a publisher needs a settlement leg and a node (config keys settle and asset)")
	}
	if (legs.Facade == nil) == (legs.Hosts == nil) {
		return nil, errors.New("a publisher needs either a facade (mode plane) or hosts (mode unicast)")
	}
	if legs.Headers == nil {
		return nil, errors.New("a publisher needs a header source: a mined object is published only once its block is in the publisher's headers")
	}
	if o.Entries < 1 {
		o.Entries = limits.PlaneEntries
	}
	if o.Signers < 1 {
		o.Signers = 1
	}
	e := &Engine{St: st, Signer: signer, Pool: pool, Legs: legs, Opts: o, Now: time.Now, Sleep: sleep,
		trees: map[string]*producer.Trees{}}
	e.kept = &producer.Kept{Load: e.load}
	e.payer = e.NewPayer()
	// Trees are settled without waiting for a block: a tree minted ahead
	// comes back as soon as the settlement leg takes it, and spend waits
	// for a proof only when a carrier or a token needs it.
	e.payer.Async = true
	for _, kind := range state.TreeKinds {
		count, ahead := o.TreeCount, o.Ahead
		if kind == state.TreeChain {
			count, ahead = o.ChainTreeCount, 0
		}
		k := kind
		lock := func(ctx context.Context) (*script.Script, error) {
			return carrier.FundingLock(ctx, signer, signer.Originator, Params(k))
		}
		e.trees[kind] = &producer.Trees{Payer: e.payer, State: treeState{e, kind}, Identity: signer.IdentityHex(),
			Count: count, Sats: o.TreeSats, Funder: "pool", Lock: lock, Change: signer.FundScript,
			Facade: unicast.Kept(), Topic: "tm_borg_kept", Ahead: ahead}
	}
	return e, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// NewPayer is a Payer over the engine's pool and legs.
func (e *Engine) NewPayer() *producer.Payer {
	p := &producer.Payer{
		Pool: e.Pool, Keys: map[string]*bwallet.Signer{e.Signer.IdentityHex(): e.Signer},
		Kept: e.kept, Settler: e.Legs.Settler, Asset: e.Legs.Asset, Fees: e.Opts.Fees,
		Timeout: e.Opts.Wait, Poll: e.Opts.Poll, Note: e.libraryNote,
	}
	if e.payer != nil {
		p.Tip = e.payer.Tip
	}
	return p
}

// libraryNote passes the library's lines on, except the one that reports a
// funding tree's publication: a borg tree is never published on its own.
func (e *Engine) libraryNote(format string, args ...any) {
	if strings.HasPrefix(format, "funding tree published") {
		return
	}
	e.note(format, args...)
}

func (e *Engine) note(format string, args ...any) {
	if e.Note != nil {
		e.Note(format, args...)
	}
}

// treeState is one kind of the home's funding trees as a
// producer.TreeState.
type treeState struct {
	e    *Engine
	kind string
}

func (t treeState) Current() *funding.Tree { return t.e.St.Set(t.kind).Tree }

func (t treeState) Adopt(tr funding.Tree) error {
	ts := t.e.St.Set(t.kind)
	ts.Tree = &tr
	ts.Trees = append(ts.Trees, tr)
	ts.Ahead = slices.DeleteFunc(ts.Ahead, func(a funding.Tree) bool { return a.Txid == tr.Txid })
	return t.e.St.Save()
}

// load rebuilds a funding tree the home keeps.
func (e *Engine) load(txid string) (*transaction.Transaction, error) {
	if t, _, ok := e.St.TreeOf(txid); ok {
		return funding.Rebuild(t.RawHex, t.BumpHex, t.BeefHex)
	}
	return nil, fmt.Errorf("transaction %s is not a funding tree this home keeps", txid)
}

// FeeError words the library's refusals to find a fee input.
func FeeError(err error) error {
	var nc *producer.NoCoinError
	var nk *producer.NoKeyError
	switch {
	case errors.As(err, &nc):
		if nc.Held > 0 {
			return fmt.Errorf("fee input: %w: the other coins are change from %d transaction(s) whose proofs have not arrived; they become spendable once mined", nc.Err, nc.Held)
		}
		return fmt.Errorf("fee input: %w (run `borg fund`)", nc.Err)
	case errors.As(err, &nk):
		return fmt.Errorf("wallet output %s is locked to a key this home does not hold", nk.Outpoint)
	}
	return err
}

// Start reads the chain tip for coinbase maturity, collects the proofs of
// change the pool holds back, and finishes whatever a previous run
// persisted and did not finish: a sweep, a transition, the wraps it owed,
// and every object not confirmed published.
func (e *Engine) Start(ctx context.Context) error {
	h, err := e.Legs.Asset.BestHeader(ctx)
	if err != nil {
		return fmt.Errorf("node tip: %w", err)
	}
	e.payer.Tip = h.Height
	CollectChange(ctx, e.Pool, e.Legs.Asset)
	var refusedErr *SweepRefusedError
	if err := e.FinishSweep(ctx); errors.As(err, &refusedErr) {
		e.note("%v", err)
	} else if err != nil {
		return err
	}
	if err := e.Resume(ctx); err != nil {
		return err
	}
	if err := e.FinishTransition(ctx); err != nil {
		return err
	}
	return e.Owed(ctx)
}

// CollectChange gives every unproven coin in the pool its proof, once its
// transaction has mined.
func CollectChange(ctx context.Context, pool *bwallet.Pool, asset *nodeapi.Asset) int {
	n := 0
	for _, id := range pool.UnprovenTxids() {
		mp, h, err := asset.Proof(ctx, id)
		if err != nil {
			continue
		}
		if k, err := pool.Prove(id, mp.Hex(), h); err == nil {
			n += k
		}
	}
	return n
}

// Resume publishes every object a previous run persisted: the same bytes,
// so a host that already holds one answers a duplicate.
func (e *Engine) Resume(ctx context.Context) error {
	for len(e.St.Outbox) > 0 {
		p := e.St.Outbox[0]
		e.note("%s %s: publishing what a previous run persisted", p.Kind, reader.Short(p.Txid))
		if err := e.publish(ctx, p); err != nil {
			return err
		}
		e.St.Done(p.Txid)
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

// Close collects a tree still being minted ahead, so its change reaches the
// pool, and records one never adopted in its kind's Ahead list, so a sweep
// finds it.
func (e *Engine) Close(ctx context.Context) error {
	for _, kind := range state.TreeKinds {
		t := e.trees[kind]
		_ = t.Wait(ctx)
		if p := t.Prepared(); p != nil {
			ts := e.St.Set(kind)
			ts.Ahead = producer.Index(ts.Ahead, p)
		}
	}
	return e.St.Save()
}

// spend returns the funding tree of kind the next carrier or first token
// spends and the output, the tree's proof cut to the minimal one a host
// admits.
func (e *Engine) spend(ctx context.Context, kind string) (*transaction.Transaction, uint32, error) {
	e.promoteAhead(kind)
	t := e.trees[kind]
	if e.hasAhead(kind) || kind == state.TreeChain {
		t.Ahead = 0
	} else {
		t.Ahead = e.Opts.Ahead
	}
	tree, vout, err := t.Spend(ctx, 1)
	if err != nil {
		return nil, 0, FeeError(fmt.Errorf("%s funding tree: %w", kind, err))
	}
	if tree.MerklePath == nil {
		if err := e.prove(ctx, kind, tree); err != nil {
			return nil, 0, err
		}
	}
	mp, err := Minimal(tree.MerklePath, tree.TxID())
	if err != nil {
		return nil, 0, fmt.Errorf("funding tree %s: %w", tree.TxID(), err)
	}
	tree.MerklePath = mp
	return tree, vout, nil
}

func (e *Engine) hasAhead(kind string) bool {
	ts := e.St.Set(kind)
	for _, t := range ts.Ahead {
		if t.IdentityKeyHex == e.Signer.IdentityHex() && t.Remaining() > 0 && (ts.Tree == nil || t.Txid != ts.Tree.Txid) {
			return true
		}
	}
	return false
}

func (e *Engine) promoteAhead(kind string) {
	ts := e.St.Set(kind)
	if ts.Tree != nil && ts.Tree.Remaining() > 0 {
		return
	}
	for _, t := range ts.Ahead {
		if t.IdentityKeyHex == e.Signer.IdentityHex() && t.Remaining() > 0 {
			_ = treeState{e, kind}.Adopt(t)
			e.note("switching to %s funding tree %s, minted ahead by an earlier run", kind, t.Txid)
			return
		}
	}
}

// prove waits for a funding tree's proof: a carrier spends only a mined
// tree and carries it with its proof (spec section 9.4).
func (e *Engine) prove(ctx context.Context, kind string, tree *transaction.Transaction) error {
	id := tree.TxID().String()
	mp, height, err := producer.Proofs{Arcade: e.Legs.Arcade, Asset: e.Legs.Asset}.Of(ctx, id)
	if err != nil {
		e.note("%s funding tree %s: waiting for it to mine", kind, id)
		if mp, height, err = e.NewPayer().Await(ctx, kind+" funding tree", tree); err != nil {
			return fmt.Errorf("%s funding tree %s: %w (it is recorded; the next command waits again)", kind, id, err)
		}
	}
	tree.MerklePath = mp
	e.kept.Prove(id, mp)
	CollectChange(ctx, e.Pool, e.Legs.Asset)
	if t, _, ok := e.St.TreeOf(id); ok {
		t.BumpHex, t.Height, t.BeefHex = mp.Hex(), height, ""
	}
	ts := e.St.Set(kind)
	for i := range ts.Trees {
		if ts.Trees[i].Txid == id {
			ts.Trees[i].BumpHex, ts.Trees[i].Height, ts.Trees[i].BeefHex = mp.Hex(), height, ""
		}
	}
	return e.St.Save()
}

// markSpent records that vout of the current tree of kind is used.
func (e *Engine) markSpent(kind string, tree *transaction.Transaction, vout uint32) {
	ts := e.St.Set(kind)
	if ts.Tree != nil && ts.Tree.Txid == tree.TxID().String() {
		ts.Tree.Next = vout + 1
		ts.Trees = producer.Index(ts.Trees, ts.Tree)
	}
}

// carrierFor mints the carrier of payload on the next funding output of
// kind, holds it to the host's rules for hall, and returns it with its
// Atomic BEEF.
func (e *Engine) carrierFor(ctx context.Context, hall, kind string, payload []byte, tree *transaction.Transaction, vout uint32) (*borgrec.Admission, []byte, error) {
	k, err := carrier.Mint(ctx, e.Signer, e.Signer.Originator, Params(kind), payload, tree, vout)
	if err != nil {
		return nil, nil, fmt.Errorf("carrier: %w", err)
	}
	beef, err := k.AtomicBEEF(false)
	if err != nil {
		return nil, nil, err
	}
	// The publisher's own copy of the host's rules: a carrier a host would
	// refuse is never persisted or published.
	a, err := borgrec.Admit(ctx, beef, nil, borgrec.Host{Hall: hall, Headers: e.Legs.Headers})
	if err != nil {
		return nil, nil, fmt.Errorf("the carrier breaks the host's rules (%s): %w", borgrec.Reason(err), err)
	}
	if len(beef) > e.Opts.ObjectBound {
		return nil, nil, fmt.Errorf("the carrier's BEEF is %d bytes, over the object bound of %d (a smaller tree_count)", len(beef), e.Opts.ObjectBound)
	}
	return a, beef, nil
}

// Retries are the waits between the tries of a submission to the facade.
var Retries = limits.Retries

// Short is a txid's first 12 digits.
func Short(txid string) string { return reader.Short(txid) }

// publish submits a persisted object by the configured mode, once its
// NotBefore has passed. A mined object reaches every host named, on the
// plane too; a carrier in unicast reaches the quorum.
func (e *Engine) publish(ctx context.Context, p state.Pending) error {
	if wait := time.Until(time.Unix(p.NotBefore, 0)); p.NotBefore > 0 && wait > 0 {
		e.note("%s %s: waiting %s, the propagation delay, so the hosts' headers have its block", p.Kind, Short(p.Txid), wait.Round(time.Second))
		if err := e.Sleep(ctx, wait); err != nil {
			return err
		}
	}
	beef, err := hex.DecodeString(p.Beef)
	if err != nil {
		return fmt.Errorf("%s %s: persisted BEEF: %w", p.Kind, p.Txid, err)
	}
	topic := borgrec.Topic(p.Hall)
	what := fmt.Sprintf("%s %s", p.Kind, Short(p.Txid))
	confirm := e.confirm(p)
	if e.Legs.Hosts != nil {
		need := 0
		if p.Mined() {
			need = len(e.Legs.Hosts.Hosts)
		}
		o := e.Legs.Hosts.Send(ctx, what, topic, beef, need, confirm)
		if err := o.Err(); err != nil {
			return fmt.Errorf("%s: publish: %w; it is persisted and the next command publishes it", what, err)
		}
		return nil
	}
	if err := e.submitFacade(ctx, what, topic, beef); err != nil {
		return err
	}
	if p.Mined() && e.Legs.Direct != nil {
		o := e.Legs.Direct.Send(ctx, what, topic, beef, len(e.Legs.Direct.Hosts), confirm)
		if err := o.Err(); err != nil {
			return fmt.Errorf("%s: a mined object reaches every host named, off the plane too: %w; it is persisted and the next command publishes it", what, err)
		}
	}
	return nil
}

func (e *Engine) submitFacade(ctx context.Context, what, topic string, beef []byte) error {
	var err error
	for i := 0; ; i++ {
		var res publish.Result
		res, err = e.Legs.Facade.Submit(ctx, topic, beef)
		if err == nil {
			if res.Duplicate {
				e.note("%s: the facade admitted nothing: it holds this already, or refused it", what)
			}
			return nil
		}
		if i == len(Retries) || ctx.Err() != nil {
			break
		}
		e.note("%s: publish failed (%v); retrying", what, err)
		if err := e.Sleep(ctx, Retries[i]); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s: publish: %w; it is persisted and the next command publishes it", what, err)
}

// confirm is the lookup that shows a host took p (spec section 12): the
// org-seq or group-seq class for a token, the wrap class for a wrap, the
// sweep class for a sweep, the member class for a grant. A grant that no
// organisation token admits yet is answered by no free class; an answer
// that admits nothing for it is counted, and confirmed by the member class
// once the token that admits it is published (ConfirmGrants).
func (e *Engine) confirm(p state.Pending) unicast.Confirm {
	if e.Legs.Reader == nil {
		return nil
	}
	var q reader.Query
	switch p.Kind {
	case state.KindOrgToken:
		q = reader.Query{"hall": p.Hall, "org": p.Org, "seq": reader.Seq(p.Seq)}
	case state.KindGroupToken:
		q = reader.Query{"hall": p.Hall, "org": p.Org, "group": p.Group, "seq": reader.Seq(p.Seq)}
	case state.KindWrap:
		q = reader.Query{"hall": p.Hall, "org": p.Org, "group": p.Group, "epoch": fmt.Sprint(p.Epoch), "member": p.Member}
	case state.KindSweep:
		q = reader.Query{"hall": p.Hall, "spent": p.Outpoint}
	case state.KindGrant:
		q = reader.Query{"hall": p.Hall, "org": p.Org, "member": p.Member}
		return func(ctx context.Context, host string) (bool, error) {
			ok, err := e.Legs.Reader.Held(ctx, host, q, p.Txid)
			if ok || err != nil {
				return ok, err
			}
			if g := e.grant(p.Txid); g != nil && !g.Admitted {
				e.note("grant %s: host %s admitted nothing, and no free class answers a grant before the roll admits it; confirmed once the organisation token is published", Short(p.Txid), host)
				return true, nil
			}
			return false, nil
		}
	default:
		return nil
	}
	return func(ctx context.Context, host string) (bool, error) {
		return e.Legs.Reader.Held(ctx, host, q, p.Txid)
	}
}

func (e *Engine) grant(txid string) *state.Grant {
	if e.St.Org == nil {
		return nil
	}
	return e.St.Org.GrantByTxid(txid)
}

// queue persists p in the outbox and publishes it, removing it once
// published.
func (e *Engine) queue(ctx context.Context, p state.Pending) error {
	if !e.St.Queued(p.Txid) {
		e.St.Outbox = append(e.St.Outbox, p)
	}
	if err := e.St.Save(); err != nil {
		return err
	}
	return e.flush(ctx, p)
}

func (e *Engine) flush(ctx context.Context, p state.Pending) error {
	if err := e.publish(ctx, p); err != nil {
		return err
	}
	e.St.Done(p.Txid)
	return e.St.Save()
}

// Txid is a hash in display order.
func Txid(h [32]byte) string { return chainhash.Hash(h).String() }

// alreadyKnown reports a node's answer for a transaction it already holds.
func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already")
}
