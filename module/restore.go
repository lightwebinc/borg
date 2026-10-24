package module

import (
	"context"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/borg/borgrec"
)

type restoreRow struct {
	o     bmodule.Output
	tx    *transaction.Transaction
	sweep bool
}

// Restore implements module.Restorer (spec section 11.4): it rebuilds the
// index. The rows from the journal, then every output of this module's
// topics the host hands over (a row handed without its BEEF is read again
// from storage with it), then every token, carrier and sweep the journal
// names that the unspent rows did not carry (a token its successor spent,
// say), and then each held token's predecessors, read through the input
// that spends them from the token as the engine stores it (the token
// alone, without its parent). A held sweep whose tombstone was spent is
// among those the journal names (its w line). Sweeps are taken in
// storage's order, so the bounded table keeps the newest. Statuses are
// functions of what is held, so the order storage returns rows in changes
// nothing else. A storage failure propagates: a restore that skipped a
// sweep would answer retracted carriers. An admission that arrives while
// it runs waits for it (the service's lock).
func (s *Service) Restore(ctx context.Context, outputs []bmodule.Output, storage bmodule.RestoreStorage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	lines, err := s.journal.Load()
	if err != nil {
		return 0, err
	}
	for i := range lines {
		s.apply(&lines[i])
	}
	s.storage = storage
	seen := map[string]bool{}
	var queue []bmodule.Output
	key := func(topic, txid string, vout uint32) string { return topic + "|" + outpoint(txid, vout) }
	for _, o := range outputs {
		k := key(o.Topic, o.Outpoint.Txid.String(), o.Outpoint.Index)
		if _, ok := s.hallOf(o.Topic); !ok || seen[k] {
			continue
		}
		seen[k] = true
		queue = append(queue, o)
	}
	type named struct {
		topic, txid string
		vout        uint32
	}
	var names []named
	for _, l := range lines {
		if !s.halls[l.Hall] {
			continue
		}
		switch l.K {
		case "c", "w":
			names = append(names, named{borgrec.Topic(l.Hall), l.Txid, 0})
		case "s":
			names = append(names, named{borgrec.Topic(l.Hall), l.S, 0})
		case "t":
			var vout uint32
			if l.Vout != nil {
				vout = uint32(*l.Vout) //nolint:gosec // an output index
			}
			names = append(names, named{borgrec.Topic(l.Hall), l.Txid, vout})
		}
	}
	for _, n := range names {
		k := key(n.topic, n.txid, n.vout)
		if seen[k] {
			continue
		}
		seen[k] = true
		h, err := chainhash.NewHashFromHex(n.txid)
		if err != nil {
			continue
		}
		o, err := storage.FindOutput(ctx, &transaction.Outpoint{Txid: *h, Index: n.vout}, n.topic)
		if err != nil {
			return 0, err
		}
		if o != nil {
			queue = append(queue, *o)
		}
	}
	read := func(o bmodule.Output) (*transaction.Transaction, error) {
		row := o
		if row.Beef == nil {
			full, err := storage.FindOutput(ctx, &row.Outpoint, row.Topic)
			if err != nil {
				return nil, err
			}
			if full != nil {
				row = *full
			}
		}
		if row.Beef == nil {
			return nil, nil
		}
		tx, err := parseFor(row.Beef, row.Outpoint.Txid)
		if err != nil {
			return nil, nil
		}
		return tx, nil
	}
	var txs []restoreRow
	for _, o := range queue {
		tx, err := read(o)
		if err != nil {
			return 0, err
		}
		if tx == nil {
			s.host.Log("ls_borg could not restore an output without its transaction", map[string]any{"topic": o.Topic, "txid": o.Outpoint.Txid.String()})
			continue
		}
		sweep := false
		if o.Outpoint.Index == 0 && len(tx.Outputs) > 0 && tx.Outputs[0] != nil && tx.Outputs[0].LockingScript != nil {
			sweep = borgrec.Classify(*tx.Outputs[0].LockingScript) == borgrec.KindFunding
		}
		txs = append(txs, restoreRow{o: o, tx: tx, sweep: sweep})
	}
	// Carriers and tokens first, sweeps after them in storage's order.
	sort.SliceStable(txs, func(i, j int) bool {
		a, b := txs[i], txs[j]
		if a.sweep != b.sweep {
			return !a.sweep
		}
		if !a.sweep {
			return false
		}
		if a.o.Score != b.o.Score {
			return a.o.Score < b.o.Score
		}
		return a.o.Outpoint.Txid.String() < b.o.Outpoint.Txid.String()
	})
	indexed := 0
	s.restoring = true
	take := func(o bmodule.Output, tx *transaction.Transaction) {
		hall, _ := s.hallOf(o.Topic)
		if s.index(hall, tx, o.Outpoint.Index, o.Score) {
			indexed++
		}
	}
	for _, r := range txs {
		take(r.o, r.tx)
	}
	// Each held token's predecessors, through the input that spends them.
	type step struct {
		hall string
		tx   *transaction.Transaction
		prev string
	}
	var walk []step
	for _, r := range txs {
		if t := s.tokenAt(r.o.Outpoint.Txid.String(), r.o.Outpoint.Index); t != nil && t.prev != "" {
			walk = append(walk, step{t.hall, r.tx, t.prev})
		}
	}
	for len(walk) > 0 {
		w := walk[len(walk)-1]
		walk = walk[:len(walk)-1]
		if s.holdsToken(w.prev) {
			continue
		}
		var in *transaction.TransactionInput
		for _, i := range w.tx.Inputs {
			if srcTxid(i) == w.prev {
				in = i
				break
			}
		}
		if in == nil {
			continue
		}
		h, err := chainhash.NewHashFromHex(w.prev)
		if err != nil {
			continue
		}
		o, err := storage.FindOutput(ctx, &transaction.Outpoint{Txid: *h, Index: in.SourceTxOutIndex}, borgrec.Topic(w.hall))
		if err != nil {
			return 0, err
		}
		if o == nil {
			continue
		}
		tx, err := read(*o)
		if err != nil {
			return 0, err
		}
		if tx == nil {
			continue
		}
		take(*o, tx)
		if t := s.tokenAt(o.Outpoint.Txid.String(), o.Outpoint.Index); t != nil && t.prev != "" {
			walk = append(walk, step{t.hall, tx, t.prev})
		}
	}
	s.restoring = false
	// Each seq's bound is applied once, here, when every token's status is
	// known.
	keys := make([]string, 0, len(s.chains))
	for k := range s.chains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ch := s.chains[k]
		s.refold(ch)
		seqs := make([]uint64, 0, len(ch.bySeq))
		for seq := range ch.bySeq {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs {
			s.bound(ch, seq)
		}
	}
	s.enforceTable()
	s.drain(ctx)
	s.flush(ctx)
	return indexed, nil
}
