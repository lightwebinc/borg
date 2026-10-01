package vectors

import (
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/carrier"

	"github.com/lightwebinc/borg/internal/borgrec"
)

// genTransaction writes transaction-v1.json, every case admitted or refused
// exactly as it says, and index-v1.json, the chain and outpoint rules an
// index applies to what was admitted.
func genTransaction(s *Scenario, files map[string][]byte) error {
	e, err := newTxEnv(s)
	if err != nil {
		return err
	}
	cases, err := e.cases(s)
	if err != nil {
		return err
	}
	var list []txJSON
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.name] {
			return fmt.Errorf("%s: two cases of one name", c.name)
		}
		seen[c.name] = true
		beef := c.beef
		if beef == nil {
			if beef, err = c.tx.AtomicBEEF(false); err != nil {
				return fmt.Errorf("%s: beef: %w", c.name, err)
			}
		}
		held := c.held
		if held == nil {
			held = e.heldInputs(c.tx)
		}
		a, err := borgrec.Admit(e.ctx, beef, held, borgrec.Host{Hall: Hall, Headers: e.c.headers, MaxSweepInputs: c.maxSweep})
		j := txJSON{Name: c.name, Note: c.note, Rule: c.rule, Txid: c.tx.TxID().String(), RawTx: c.tx.Hex(),
			BEEF: hx(beef), Coins: []coinJSON{}, MaxSweepInputs: c.maxSweep, Scripts: scriptsVerify(e.ctx, c.tx)}
		for _, i := range held {
			in := c.tx.Inputs[i]
			j.Coins = append(j.Coins, coinJSON{InputIndex: i, Txid: in.SourceTXID.String(), Vout: in.SourceTxOutIndex})
		}
		if !j.Scripts {
			return fmt.Errorf("%s: its own scripts do not verify", c.name)
		}
		switch {
		case c.reason == "" && err != nil:
			return fmt.Errorf("%s: refused: %w", c.name, err)
		case c.reason != "" && err == nil:
			return fmt.Errorf("%s: admitted, want %s", c.name, c.reason)
		case c.reason != "" && borgrec.Reason(err) != c.reason:
			return fmt.Errorf("%s: reason %s, want %s (%v)", c.name, borgrec.Reason(err), c.reason, err)
		case err == nil:
			if a.Txid != [32]byte(*c.tx.TxID()) {
				return fmt.Errorf("%s: admitted another subject", c.name)
			}
			j.Verdict, j.Admits = "admit", admission(a)
		default:
			j.Verdict, j.Reason = "refuse", c.reason
		}
		list = append(list, j)
	}
	var headers []map[string]any
	for ht := uint32(801); ht <= e.c.height; ht++ {
		headers = append(headers, map[string]any{"height": ht, "merkleRoot": e.c.headers[ht].String()})
	}

	// Stored and replayed: each admitted token as the engine stores it (an
	// Atomic BEEF of the token alone), read back without its parent, and the
	// BEEF a replayer assembles from two stored tokens (or the chain tree at
	// seq 0), byte for byte the BEEF the token was first admitted in.
	type storedJSON struct {
		Name     string `json:"name"`
		Txid     string `json:"txid"`
		Stored   string `json:"stored"`
		Seq      uint64 `json:"seq"`
		Kind     string `json:"kind"`
		Record   string `json:"record"`
		Parent   string `json:"parentTxid"`
		Replayed string `json:"replayBeef"`
	}
	var stored []storedJSON
	for _, st := range []struct {
		name          string
		token, parent *transaction.Transaction
	}{
		{"org-token-genesis", e.org[0], e.chain}, {"org-token-revoke", e.org[1], e.org[0]},
		{"group-token-create", e.group[0], e.chain}, {"group-token-add", e.group[1], e.group[0]},
		{"group-token-remove-same-block", e.group[2], e.group[1]},
	} {
		sb, err := st.token.AtomicBEEF(false)
		if err != nil {
			return err
		}
		tx, tok, err := borgrec.StoredToken(sb)
		if err != nil {
			return fmt.Errorf("%s: stored: %w", st.name, err)
		}
		var ptx *transaction.Transaction
		if tok.Seq() == 0 {
			ptx = st.parent // the chain tree, from the publisher or a chain source
		} else {
			pb, err := st.parent.AtomicBEEF(false)
			if err != nil {
				return err
			}
			if ptx, _, err = borgrec.StoredToken(pb); err != nil {
				return err
			}
		}
		replay, err := borgrec.TokenBEEF(tx, ptx)
		if err != nil {
			return err
		}
		var want string
		for _, j := range list {
			if j.Name == st.name {
				want = j.BEEF
			}
		}
		if hx(replay) != want {
			return fmt.Errorf("%s: the replayed BEEF is not the one admitted", st.name)
		}
		stored = append(stored, storedJSON{Name: st.name, Txid: tx.TxID().String(), Stored: hx(sb), Seq: tok.Seq(),
			Kind: string(tok.Kind), Record: hx(tok.Record), Parent: ptx.TxID().String(), Replayed: hx(replay)})
	}
	if err := put(files, "transaction-v1.json", map[string]any{
		"description": "Transactions admitted or refused by the topic manager's rules (docs/spec.md section 11.1), in the order given there. " +
			"Each case is the transaction as raw hex and the BEEF submitted (Atomic BEEF unless the case says otherwise: an unmined carrier's with its funding tree, a mined sweep's with its own proof; a chain token's is BEEF V1 of its parent then the token, as borgrec.TokenBEEF writes it), " +
			"and the previous coins it is admitted with: the inputs that spend an output the topic holds once the admitted cases are admitted (inputIndex), each with the outpoint it spends (txid in display order). " +
			"Admission runs against the hall and the headers below, with a BEEF bound of maxBeef bytes and a sweep input bound of maxSweepInputs unless a case names its own. " +
			"A refusal is raised, never answered with empty instructions. admits lists the admittance instructions (outputsToAdmit, coinsToRetain) and what the lookup service records: a carrier's commitment (hash byte order), record, owner and funding outpoint; a token's seq, record, parent txid (display order) and its predecessor's seq; a sweep's spent outpoints. " +
			"scriptsVerify says every input of the transaction passes the script interpreter against the output it spends, so each refusal is made by the rule the case names and by no earlier one. rule names the frozen-list row a refusal exercises. Signatures are deterministic (RFC 6979). " +
			"stored is each admitted token as the engine stores it, the token alone with its proof; a replayer assembles replayBeef from two stored tokens (at seq 0, the token and its chain tree), byte for byte the BEEF the token was first admitted in.",
		"hall":           Hall,
		"topic":          borgrec.Topic(Hall),
		"maxBeef":        borgrec.DefaultMaxBEEF,
		"maxSweepInputs": borgrec.DefaultMaxSweepInputs,
		"headers":        headers,
		"cases":          list,
		"stored":         stored,
	}); err != nil {
		return err
	}
	return genIndex(e, files)
}

// genIndex writes index-v1.json: the fold of each real chain, with the
// status of each token, and outpoint rows with each carrier's status.
func genIndex(e *txEnv, files map[string][]byte) error {
	type tokJSON struct {
		Seq    uint64 `json:"seq"`
		Txid   string `json:"txid"`
		Status string `json:"status"`
	}
	of := borgrec.NewOrgFold()
	var orgs []tokJSON
	for i, o := range e.orgRecs {
		st := of.Apply(o)
		if st != borgrec.StatusConsistent {
			return fmt.Errorf("org %d: %s", i, st)
		}
		orgs = append(orgs, tokJSON{o.Seq, e.org[i].TxID().String(), st})
	}
	gf := borgrec.NewGroupFold()
	var groups []tokJSON
	for i, g := range e.groupRec {
		st := gf.Apply(g)
		if st != borgrec.StatusConsistent {
			return fmt.Errorf("group %d: %s", i, st)
		}
		groups = append(groups, tokJSON{g.Seq, e.group[i].TxID().String(), st})
	}

	// Chains that break the fold, each folded from seq 0.
	type chainJSON struct {
		Label    string   `json:"label"`
		Kind     string   `json:"kind"`
		Records  []string `json:"records"`
		Statuses []string `json:"statuses"`
	}
	var broken []chainJSON
	enc := func(v interface{ Encode() ([]byte, error) }) string {
		b, err := v.Encode()
		if err != nil {
			panic(err)
		}
		return hx(b)
	}
	{
		o1 := *e.orgRecs[1]
		o1.Revoke = sorted32(label("commitment/never-admitted"))
		var recs []string
		f := borgrec.NewOrgFold()
		sts := []string{f.Apply(e.orgRecs[0]), f.Apply(&o1)}
		recs = append(recs, enc(e.orgRecs[0]), enc(&o1))
		broken = append(broken, chainJSON{"seq 1 revokes a commitment the roll does not hold", "org", recs, sts})
	}
	{
		g := e.groupRec
		g3 := *g[2]
		g3.Seq, g3.Prev, g3.Remove, g3.Created = 3, label("txid/group/2"), nil, g3.Created+600
		g3.Epoch, g3.EpochSeq, g3.Commitment = 2, 3, g[0].Commitment
		f := borgrec.NewGroupFold()
		sts := []string{f.Apply(g[0]), f.Apply(g[1]), f.Apply(g[2]), f.Apply(&g3)}
		broken = append(broken, chainJSON{"seq 3 begins epoch 2 with epoch 0's commitment", "group",
			[]string{enc(g[0]), enc(g[1]), enc(g[2]), enc(&g3)}, sts})
		g3b := *g[2]
		g3b.Seq, g3b.Prev, g3b.Remove, g3b.Add, g3b.Count, g3b.Created = 3, label("txid/group/2"), nil, sortedKeys(Bob), 3, g3b.Created+600
		g3b.Members = borgrec.MembersRoot(sortedKeys(Alice, Bob, Carol))
		f = borgrec.NewGroupFold()
		sts = []string{f.Apply(g[0]), f.Apply(g[1]), f.Apply(g[2]), f.Apply(&g3b)}
		broken = append(broken, chainJSON{"seq 3 adds bob back in epoch 1, removed at seq 2", "group",
			[]string{enc(g[0]), enc(g[1]), enc(g[2]), enc(&g3b)}, sts})
		g3c := g3b
		g3c.Members = borgrec.MembersRoot(sortedKeys(Alice, Carol))
		f = borgrec.NewGroupFold()
		g4 := g3c
		g4.Seq, g4.Prev, g4.Add, g4.Count = 4, label("txid/group/3"), nil, 3
		sts = []string{f.Apply(g[0]), f.Apply(g[1]), f.Apply(g[2]), f.Apply(&g3c), f.Apply(&g4)}
		broken = append(broken, chainJSON{"seq 3's members root is not the fold's; every later token is unusable", "group",
			[]string{enc(g[0]), enc(g[1]), enc(g[2]), enc(&g3c), enc(&g4)}, sts})
	}
	for _, b := range broken {
		if b.Statuses[len(b.Statuses)-1] == borgrec.StatusConsistent {
			return fmt.Errorf("fold %q: not broken", b.Label)
		}
	}

	// Outpoint rows: alice's outpoint with her carrier and a second carrier
	// on it (carrier-locktime-max's), bob's swept outpoint.
	type rowCarrier struct {
		Commitment string `json:"commitment"`
		Status     string `json:"status"`
	}
	type rowJSON struct {
		Label         string       `json:"label"`
		Outpoint      string       `json:"outpoint"`
		WinnerDropped bool         `json:"winnerDropped"`
		Swept         bool         `json:"swept"`
		Winner        string       `json:"winner"`
		Carriers      []rowCarrier `json:"carriers"`
	}
	second := cloneTx(e.carriers["alice"])
	second.LockTime, second.Inputs[0].SequenceNumber = 0xffffffff, 0xfffffffe
	second = signAll(second)
	var rows []rowJSON
	for _, r := range []struct {
		label   string
		vout    uint32
		cs      [][32]byte
		dropped bool
		swept   bool
	}{
		{"alice's outpoint: her carrier and a second one", 0, [][32]byte{carrier.Commitment(e.carriers["alice"]), carrier.Commitment(second)}, false, false},
		{"the same, the winner dropped", 0, [][32]byte{carrier.Commitment(e.carriers["alice"]), carrier.Commitment(second)}, true, false},
		{"bob's outpoint, swept", 1, [][32]byte{carrier.Commitment(e.carriers["bob"])}, false, true},
	} {
		row := &borgrec.OutpointRow{Commitments: r.cs, WinnerDropped: r.dropped, Swept: r.swept}
		w, _ := row.Winner()
		j := rowJSON{Label: r.label, Outpoint: fmt.Sprintf("%s.%d", e.grant.TxID().String(), r.vout), WinnerDropped: r.dropped, Swept: r.swept, Winner: hx(w[:])}
		for _, c := range r.cs {
			j.Carriers = append(j.Carriers, rowCarrier{hx(c[:]), row.Status(c)})
		}
		rows = append(rows, j)
	}
	return put(files, "index-v1.json", map[string]any{
		"description": "What the index computes from admitted objects (docs/spec.md section 11.2), never at admission. " +
			"orgChain and groupChain are the transaction vectors' chains folded from seq 0, each token consistent. " +
			"broken are chains folded from seq 0 whose last token breaks a fold rule: roll-mismatch, members-mismatch (sticky: every later token is unusable), commitment-reuse, rejoin-unrotated. " +
			"outpoints are funding-outpoint rows: the winner is the lowest commitment in hash byte order; a carrier is retracted when a sweep spends the outpoint, superseded when it is not the winner or the winner was dropped, held otherwise.",
		"orgChain":   orgs,
		"groupChain": groups,
		"broken":     broken,
		"outpoints":  rows,
	})
}
