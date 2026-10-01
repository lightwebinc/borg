package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"

	"github.com/lightwebinc/borg/internal/state"
)

// Retraction and revocation (spec sections 5.6 and 9.5): a sweep spends
// funding-tree outputs, used or not, with bcommon carrier.Sweep, whose
// output 0 is a funding-shaped tombstone, so a host admits it. Once it
// mines, every carrier that spent one of those outputs is a double spend:
// every conforming host retracts it, and a grant among them is revoked for
// every BRC-52 verifier. A sweep is persisted before it is settled, waited
// for until it mines, and published once, after the propagation delay, to
// the plane and to every host named. Only honest hosts drop what a sweep
// retracts: it is not erasure.

// Retract builds and persists one sweep of vouts of the funding tree txid,
// with its fee from the pool, then settles and publishes it in hall.
func (e *Engine) Retract(ctx context.Context, hall, txid string, vouts []uint32) (*state.Sweep, error) {
	if e.St.InFlight() != nil {
		var refusedErr *SweepRefusedError
		if err := e.FinishSweep(ctx); errors.As(err, &refusedErr) {
			e.note("%v", err)
		} else if err != nil {
			return nil, err
		}
	}
	if len(vouts) == 0 {
		return nil, errors.New("a sweep spends at least one output")
	}
	var left []uint32
	for _, v := range vouts {
		if e.SweptBy(txid, v) == nil {
			left = append(left, v)
		}
	}
	if len(left) == 0 {
		return e.SweptBy(txid, vouts[0]), nil
	}
	vouts = left
	_, kind, ok := e.St.TreeOf(txid)
	if !ok {
		return nil, fmt.Errorf("transaction %s is not a funding tree this home keeps", txid)
	}
	tree, err := e.kept.Tx(txid)
	if err != nil {
		return nil, err
	}
	payer := e.NewPayer()
	fee, coin, err := e.sweepFee(ctx, payer)
	if err != nil {
		return nil, FeeError(fmt.Errorf("sweep fee: %w", err))
	}
	change, err := e.Signer.FundScript()
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	tx, err := carrier.Sweep(ctx, e.Signer, e.Signer.Originator, Params(kind), tree, vouts, fee.Tx, fee.Vout, fee.Unlocker, change,
		e.Opts.Fees.SatPerByte, e.Opts.Fees.Floor)
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	sw := state.Sweep{Tree: txid, Vouts: slices.Clone(vouts), Hall: hall, Txid: tx.TxID().String(), RawHex: tx.Hex(), Fee: coin}
	e.St.Sweeps = append(e.St.Sweeps, sw)
	if t := e.St.Set(kind).Tree; t != nil && t.Txid == txid {
		// The rest of the tree stays usable only past every output swept.
		if last := slices.Max(vouts); last >= t.Next {
			t.Next = last + 1
		}
	}
	if err := e.St.Save(); err != nil {
		payer.GiveBack()
		return nil, err
	}
	if err := e.FinishSweep(ctx); err != nil {
		return &e.St.Sweeps[len(e.St.Sweeps)-1], err
	}
	return &e.St.Sweeps[len(e.St.Sweeps)-1], nil
}

// sweepFee takes the sweep's fee coin, and the coin as the pool held it,
// so a refused sweep can give it back. When every coin left is change from
// a transaction not yet mined, it waits up to Opts.Wait for one.
func (e *Engine) sweepFee(ctx context.Context, payer *producer.Payer) (mint.Input, *bwallet.Output, error) {
	var deadline time.Time
	for {
		before := e.Pool.Outputs()
		fee, err := payer.Take(ctx)
		var nc *producer.NoCoinError
		if errors.As(err, &nc) && nc.Held > 0 {
			if deadline.IsZero() {
				deadline = time.Now().Add(e.Opts.Wait)
				e.note("fee: every coin is change from %d transaction(s) not yet mined; waiting for one", nc.Held)
			}
			if time.Now().After(deadline) {
				return mint.Input{}, nil, err
			}
			poll := e.Opts.Poll
			if poll <= 0 {
				poll = producer.DefaultPoll
			}
			if err := e.Sleep(ctx, poll); err != nil {
				return mint.Input{}, nil, err
			}
			CollectChange(ctx, e.Pool, e.Legs.Asset)
			continue
		}
		if err != nil {
			return mint.Input{}, nil, err
		}
		op := fmt.Sprintf("%s.%d", fee.Tx.TxID(), fee.Vout)
		for i := range before {
			if before[i].Outpoint() == op {
				return fee, &before[i], nil
			}
		}
		return fee, nil, nil
	}
}

// FinishSweep takes the sweep in flight, if any, through settlement, its
// proof and its publication. A sweep the network definitively refuses is
// marked failed, its fee coin given back when the node shows it unspent,
// and a *SweepRefusedError returned. Any other failure leaves it in flight.
func (e *Engine) FinishSweep(ctx context.Context) error {
	sw := e.St.InFlight()
	if sw == nil {
		return nil
	}
	tx, err := funding.Rebuild(sw.RawHex, sw.BumpHex, "")
	if err != nil {
		return fmt.Errorf("sweep %s: %w", sw.Txid, err)
	}
	if sw.BumpHex == "" {
		payer := e.NewPayer()
		if !sw.Submitted {
			if err := e.sources(ctx, tx); err != nil {
				return fmt.Errorf("sweep %s: %w (it is persisted; the next command sends it again)", sw.Txid, err)
			}
			if err := e.Legs.Settler.Submit(ctx, tx); err != nil && !alreadyKnown(err) {
				if why := e.refusal(ctx, tx, err); why != "" {
					return e.failSweep(ctx, sw, why)
				}
				return fmt.Errorf("sweep %s: settle via %s: %w (it is persisted; the next command sends it again)", sw.Txid, e.Legs.Settler.Name(), err)
			}
			sw.Submitted = true
			if err := e.St.Save(); err != nil {
				return err
			}
		}
		if why := e.refusal(ctx, tx, nil); why != "" {
			return e.failSweep(ctx, sw, why)
		}
		mp, height, err := payer.Await(ctx, "sweep", tx)
		if err != nil {
			if why := e.refusal(ctx, tx, nil); why != "" {
				return e.failSweep(ctx, sw, why)
			}
			return fmt.Errorf("sweep %s: %w (the next command waits again)", sw.Txid, err)
		}
		sw.BumpHex, sw.Height = mp.Hex(), height
		payer.Change(tx, height, mp)
		if err := e.St.Save(); err != nil {
			return err
		}
		tx.MerklePath = mp
	}
	mp, err := Minimal(tx.MerklePath, tx.TxID())
	if err != nil {
		return fmt.Errorf("sweep %s: %w", sw.Txid, err)
	}
	tx.MerklePath = mp
	if err := e.inHeaders(ctx, "sweep", tx); err != nil {
		return err
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		return err
	}
	p := state.Pending{Kind: state.KindSweep, Hall: sw.Hall, Txid: sw.Txid, Beef: hex.EncodeToString(beef),
		Outpoint: fmt.Sprintf("%s.%d", sw.Tree, sw.Vouts[0]), NotBefore: e.Now().Add(e.Opts.Delay).Unix()}
	sw.Done = true
	if o := e.St.Org; o != nil {
		for i := range o.Grants {
			g := &o.Grants[i]
			if g.Tree == sw.Tree && slices.Contains(sw.Vouts, g.Vout) {
				g.Swept = sw.Txid
			}
		}
	}
	return e.queue(ctx, p)
}

// SweptBy is the sweep that spends output vout of the funding tree txid,
// or nil. A failed sweep spends nothing.
func (e *Engine) SweptBy(txid string, vout uint32) *state.Sweep {
	for i := range e.St.Sweeps {
		if sw := &e.St.Sweeps[i]; sw.Failed == "" && sw.Tree == txid && slices.Contains(sw.Vouts, vout) {
			return sw
		}
	}
	return nil
}

// sources gives every input of a transaction rebuilt from its raw bytes the
// transaction it spends: a settlement leg takes Extended Format (BRC-30).
// A funding tree or chain token is the home's own; anything else is read
// from the node.
func (e *Engine) sources(ctx context.Context, tx *transaction.Transaction) error {
	for i, in := range tx.Inputs {
		if in.SourceTransaction != nil {
			continue
		}
		id := in.SourceTXID.String()
		src, err := e.kept.Tx(id)
		if err != nil {
			src, err = e.ownToken(id)
		}
		if err != nil {
			if e.Legs.Asset == nil {
				return fmt.Errorf("input %d: %s is not kept and no node is configured to read it from", i, id)
			}
			raw, ferr := e.Legs.Asset.TxRaw(ctx, id)
			if ferr != nil {
				return fmt.Errorf("input %d: reading %s from the node: %w", i, id, ferr)
			}
			if src, err = guard.ParseTransaction(raw, guard.DefaultBound); err != nil {
				return fmt.Errorf("input %d: %s from the node: %w", i, id, err)
			}
		}
		if src.TxID().String() != id || int(in.SourceTxOutIndex) >= len(src.Outputs) {
			return fmt.Errorf("input %d: the transaction found for %s is not the one it spends", i, id)
		}
		in.SourceTransaction = src
	}
	return nil
}

// Tx is a funding tree the home keeps, rebuilt.
func (e *Engine) Tx(txid string) (*transaction.Transaction, error) { return e.kept.Tx(txid) }
