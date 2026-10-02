package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clients "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/reader"
)

// target is what a reading command reads: a hall, an organisation, and the
// reader over the configured hosts and headers.
type target struct {
	rd   *reader.Client
	hall string
	org  []byte
}

// readTarget resolves the organisation (the -org flag or config key, else
// the organisation this home runs) and its hall.
func (g *global) readTarget(_ context.Context) (*target, error) {
	rd, err := g.reader()
	if err != nil {
		return nil, err
	}
	orgHex, hall := g.cfg.Org, g.cfg.Hall
	if e, err := g.openWallet(); err == nil {
		if st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex()); err == nil && st.Org != nil {
			if orgHex == "" || orgHex == st.Identity {
				orgHex = st.Identity
				if hall == "" {
					hall = st.Org.Hall
				}
			}
		}
	}
	if orgHex == "" {
		return nil, usage("no organisation: set org (or -org) to the organisation's identity key")
	}
	org, _, err := parseKey("org", orgHex)
	if err != nil {
		return nil, err
	}
	g.cfg.Hall = hall
	if hall, err = g.hall(); err != nil {
		return nil, err
	}
	return &target{rd: rd, hall: hall, org: org}, nil
}

// reportChain prints how the hosts' answers compare with the canonical
// chain, and returns a problem when they disagree or one refused.
func (g *global) reportChain(what string, v *reader.ChainView) error {
	var problem error
	for h, err := range v.HostErr {
		g.say("host %s: NOT ASKED: %s", h, firstLine(err.Error()))
		problem = incomplete("%d of %d host(s) answered about the %s", len(g.cfg.Hosts)-len(v.HostErr), len(g.cfg.Hosts), what)
	}
	for _, r := range v.Refused {
		g.say("host %s: REFUSED token %s (%s): %v", r.Host, r.Txid, r.Reason, r.Err)
		problem = refused("a host answered a token that does not verify; it is not used")
	}
	for h, seqs := range v.Lacks {
		if len(seqs) == 0 {
			continue
		}
		g.say("host %s: DISAGREES: it does not answer seq %s of the %s, which another host answered", h, reader.JoinSeqs(seqs), what)
		if problem == nil {
			problem = incomplete("the hosts disagree about the %s; the canonical chain is read from those that answer it", what)
		}
	}
	for _, f := range v.Forks {
		g.say("token %s at seq %d verifies but does not continue the canonical chain (a fork or an orphan); it is not used", f.Txid, f.Seq())
	}
	if u := v.Usable(); u < len(v.Tokens) {
		g.say("the %s folds to %s at seq %d: UNUSABLE from that token on (fail closed)", what, v.Statuses[u], u)
	}
	for i, s := range v.Statuses {
		if s == borgrec.StatusCommitmentReuse || s == borgrec.StatusRejoinUnrotated {
			g.say("the %s is INCONSISTENT at seq %d: %s", what, i, s)
		}
	}
	return problem
}

// ratchet holds a chain to the latest token this home verified before: a
// host may not answer an older state as the latest.
func (g *global) ratchet(st *state.State, hall, org, chain string, v *reader.ChainView) error {
	if st == nil {
		return nil
	}
	if s := st.SeenOf(hall, org, chain); s != nil {
		if uint64(len(v.Tokens)) <= s.Seq {
			return incomplete("the hosts answer the chain to seq %d, and this home verified seq %d (%s): an older state; ask other hosts", v.Latest().Seq(), s.Seq, s.Txid)
		}
		if v.Tokens[s.Seq].Txid != s.Txid {
			g.say("the token at seq %d is %s, and this home verified %s: a reorganisation, or a fork", s.Seq, v.Tokens[s.Seq].Txid, s.Txid)
		}
	}
	t := v.Latest()
	st.See(state.Seen{Hall: hall, Org: org, Chain: chain, Seq: t.Seq(), Txid: t.Txid})
	return nil
}

// resolveGroup names a group by its id or, through the groups class, by its
// name.
func (g *global) resolveGroup(ctx context.Context, t *target, s string) (string, error) {
	if _, err := reader.ParseGroup(s); err == nil {
		return s, nil
	}
	ids, _ := t.rd.GroupsAll(ctx, t.hall, t.org)
	var found []string
	for _, id := range ids {
		v, err := t.rd.Group(ctx, t.hall, t.org, id)
		if err == nil && v.Token.Name == s {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		return "", incomplete("no host answers a group named %q of the organisation; name it by its id", termsafe.Abbrev(s))
	case 1:
		return found[0], nil
	}
	return "", usage("%d groups are named %q; name it by its id (%s)", len(found), termsafe.Abbrev(s), strings.Join(found, ", "))
}

const openHelp = `usage: borg open <group> [-org KEY] [-write] [-show-key]

Open, as a member, the group's epoch keys this home's key can open, and say
which it cannot. The group's chain is read from every host and folded from
seq 0 against this home's headers; then, for every epoch, in the order of
spec section 6.4: the token that began the epoch (token), a wrap record
that names this key (entry), the wallet's decryption (undecryptable), and
the epoch's commitment (commitment). A key that opens is kept in the home,
so later reads need no host. An epoch this key was never a member of is
named and not asked about.

Exit 0 when the current epoch's key is held, 1 when this key cannot open it
(it is not a member: never added, or removed), 3 when no host answered.

-write is a writer's check (spec section 6.5): positive evidence, at most
freshness old (default 60s), that the group's latest token is the latest:
its proof verifies against this home's headers and the node (config key
asset) says its output is unspent. Without it a writer must not write
(exit 3). The evidence is recorded in the home. -show-key prints the
current epoch key in hex: a secret.`

func cmdOpen(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("open", openHelp)
	write := fs.Bool("write", false, "require writer evidence that the latest token is unspent")
	show := fs.Bool("show-key", false, "print the current epoch key (a secret)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usage("open <group>")
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	gid, err := g.resolveGroup(ctx, t, pos[0])
	if err != nil {
		return err
	}
	gv, err := t.rd.Group(ctx, t.hall, t.org, gid)
	if err != nil {
		if errors.Is(err, reader.ErrNoChain) {
			return incomplete("group %s: %v", gid, err)
		}
		return refused("group %s: %v", gid, err)
	}
	problem := g.reportChain("group "+gid, gv.ChainView)
	orgHex := hex.EncodeToString(t.org)
	if err := g.ratchet(h.st, t.hall, orgHex, gid, gv.ChainView); err != nil {
		return err
	}
	me := h.e.Signer().Identity.Compressed()
	cur := gv.Epoch()
	fmt.Fprintf(g.stdout, "group   %s %s\nlatest  seq %d %s epoch %d, %d member(s)\n", gid, termsafe.Text(gv.Token.Name), gv.Token.Seq, gv.Latest().Txid, cur, len(gv.Members))
	if gv.Rejoined {
		g.say("the group is INCONSISTENT: a token re-added a removed key without a new epoch; nothing keyed under that epoch is hidden from the returning key")
	}
	mine := map[uint32]bool{}
	for _, e := range gv.EpochsOf(me) {
		mine[e] = true
	}
	var curErr error
	for e := uint32(0); e <= cur; e++ {
		if gv.Begins[e] == nil {
			continue
		}
		if !mine[e] {
			why := "not a member of this epoch"
			if e == cur {
				why = notMemberWhy(gv, me)
				curErr = refused("this key cannot open epoch %d of group %s: %s", cur, gid, why)
			}
			fmt.Fprintf(g.stdout, "epoch   %d CANNOT OPEN: %s\n", e, why)
			continue
		}
		if k := h.st.KeyFor(t.hall, orgHex, gid, e); k != nil {
			fmt.Fprintf(g.stdout, "epoch   %d held (commitment %s)\n", e, k.Commitment[:16])
			continue
		}
		op := t.rd.Open(ctx, h.e.Signer(), me, gv, e)
		if op.Label != "" {
			fmt.Fprintf(g.stdout, "epoch   %d CANNOT OPEN (%s): %s\n", e, op.Label, termsafe.Text(firstLine(op.Err.Error())))
			if e == cur {
				if op.Label == "entry" {
					curErr = incomplete("no host answers a wrap of epoch %d to this key yet: the organisation publishes it after the token mines; ask again", e)
				} else {
					curErr = refused("epoch %d does not open (%s): %v", e, op.Label, op.Err)
				}
			}
			continue
		}
		h.st.AddKey(state.EpochKey{Hall: t.hall, Org: orgHex, Group: gid, Epoch: e, EpochID: hex.EncodeToString(op.EpochID[:]),
			Commitment: hex.EncodeToString(op.Commitment[:]), Key: hex.EncodeToString(op.Key[:])})
		fmt.Fprintf(g.stdout, "epoch   %d OPENED from %s (commitment %s)\n", e, op.Host, hex.EncodeToString(op.Commitment[:8]))
	}
	if err := h.st.Save(); err != nil {
		return err
	}
	if curErr != nil {
		return curErr
	}
	if *show {
		k := h.st.KeyFor(t.hall, orgHex, gid, cur)
		fmt.Fprintf(g.stdout, "key     %s\n", k.Key)
	}
	if *write {
		if err := g.writerEvidence(ctx, h.st, gv); err != nil {
			return err
		}
	}
	if problem != nil {
		var ee *exitError
		if errors.As(problem, &ee) && ee.code == exitIncomplete {
			// The key opened from the canonical chain: the disagreement is
			// reported, and does not undo it.
			return nil
		}
	}
	return problem
}

// notMemberWhy says why a key is not a member of the group's current
// epoch: the token that removed it, or that it was never added.
func notMemberWhy(gv *reader.GroupView, me []byte) string {
	for i := len(gv.Tokens) - 1; i >= 0; i-- {
		for _, r := range gv.Tokens[i].Token.Group.Remove {
			if bytes.Equal(r, me) {
				return fmt.Sprintf("removed at seq %d (%s), which began epoch %d; no wrap of a later epoch names this key", i, gv.Tokens[i].Txid, gv.Tokens[i].Token.Group.Epoch)
			}
		}
	}
	return "never a member of the group"
}

// writerEvidence is the writer's check of spec section 6.5: the home's own
// view (the latest token this home verified, never older), the proof
// against this home's headers, and the node's positive answer that the
// token's output is unspent, at most freshness old.
func (g *global) writerEvidence(ctx context.Context, st *state.State, gv *reader.GroupView) error {
	t := gv.Latest()
	orgHex := hex.EncodeToString(gv.Org)
	now := time.Now()
	if ev := st.EvidenceOf(gv.Hall, orgHex, gv.Chain); ev != nil && ev.Txid == t.Txid && now.Sub(time.Unix(ev.At, 0)) <= g.cfg.Freshness {
		fmt.Fprintf(g.stdout, "write   under epoch %d: evidence %s old (%s)\n", ev.Epoch, now.Sub(time.Unix(ev.At, 0)).Round(time.Second), ev.Source)
		return nil
	}
	if o := st.Org; o != nil && o.Pending != nil && o.Pending.Group == gv.Chain && o.Pending.Key != "" {
		return incomplete("this home's own transition %s, not yet mined, begins epoch %d of the group: writers wait until it mines and its key is wrapped", o.Pending.Txid, o.Pending.Epoch)
	}
	var raw func(ctx context.Context, txid string) ([]byte, error)
	sp := g.spends()
	if g.cfg.Asset != "" {
		_, asset, _ := g.node(false)
		raw = asset.TxRaw
	}
	f, err := reader.Latest(ctx, sp, raw, gv, now)
	if err != nil || !f.OK() {
		if err == nil {
			err = errors.New("no positive evidence")
		}
		return incomplete("no writer evidence for group %s: %v; a writer does not write (spec section 6.5)", gv.Chain, err)
	}
	src := "unspent per the node " + g.cfg.Asset
	if f.Next != nil {
		src = fmt.Sprintf("spent by the next token %s, not yet mined, which keeps the epoch", reader.Short(f.By))
	}
	st.Record(state.Evidence{Hall: gv.Hall, Org: orgHex, Group: gv.Chain, Seq: t.Seq(), Txid: t.Txid, Epoch: gv.Epoch(), At: now.Unix(), Source: src})
	if err := st.Save(); err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "write   under epoch %d: the latest token %s verified and %s, now\n", gv.Epoch(), reader.Short(t.Txid), src)
	return nil
}

const rollHelp = `usage: borg roll [-org KEY]

Read the organisation's roll (spec section 13): its chain from every host,
folded from seq 0 against this home's headers; then the roll class from
every host, all pages, every grant verified (the carrier rules, its
certificate, its reveal), and the root of the commitments each host
answered compared with the latest token's roll. A root that differs means
the host withheld or added a grant (exit 3). Each grant is judged: in the
roll, scope grants member, not expired, revealed unless the organisation is
opaque, and its revocation outpoint unspent per the node (asset). Every
field someone else wrote is filtered before it reaches the terminal.`

func cmdRoll(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("roll", rollHelp)
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("roll takes no arguments")
	}
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	ov, err := t.rd.Org(ctx, t.hall, t.org)
	if err != nil {
		if errors.Is(err, reader.ErrNoChain) {
			return incomplete("the organisation: %v", err)
		}
		return refused("the organisation: %v", err)
	}
	problem := g.reportChain("organisation chain", ov.ChainView)
	fmt.Fprintf(g.stdout, "org     %s %s\nlatest  seq %d %s, %d grant(s), roll %s, opaque %v\n", hex.EncodeToString(t.org), termsafe.Text(ov.Token.Name),
		ov.Token.Seq, ov.Latest().Txid, ov.Token.Count, hex.EncodeToString(ov.Token.Roll[:8]), ov.Token.Opaque)
	answers := t.rd.Roll(ctx, ov)
	printed := map[string]bool{}
	now := time.Now()
	sp := g.spends()
	for _, a := range answers {
		switch {
		case a.Err != nil:
			g.say("host %s: NOT ASKED: %s", a.Host, firstLine(a.Err.Error()))
			problem = incomplete("a host could not be asked")
		case !a.Matches:
			g.say("host %s: its roll (%d grant(s)) does not reproduce the root of seq %d: it withheld or added a grant", a.Host, len(a.Grants), ov.Token.Seq)
			if problem == nil {
				problem = incomplete("a host's roll does not reproduce the latest token's root")
			}
		default:
			g.say("host %s: %d grant(s), the root of seq %d reproduced", a.Host, len(a.Grants), ov.Token.Seq)
		}
		for _, r := range a.Refused {
			g.say("host %s: REFUSED grant %s (%s): %v", r.Host, r.Txid, r.Reason, r.Err)
			problem = refused("a host answered a grant that does not verify; it is not shown")
		}
		for _, car := range a.Grants {
			if printed[car.Txid] {
				continue
			}
			printed[car.Txid] = true
			st := reader.Judge(ctx, car, ov, sp, now)
			fmt.Fprintln(g.stdout, grantLine(st))
		}
	}
	return problem
}

// grantLine prints one judged grant, every field someone else wrote
// filtered.
func grantLine(st reader.GrantStatus) string {
	gr := st.Grant
	line := fmt.Sprintf("grant   %s %s", st.Car.Txid, hex.EncodeToString(gr.Subject))
	if st.Values != nil {
		line += fmt.Sprintf(" role %s scope %s until %s", termsafe.Abbrev(st.Values["role"]), termsafe.Abbrev(st.Values["scope"]), termsafe.Text(st.Values["expiry"]))
	} else {
		line += " (opaque: fields unrevealed)"
	}
	if st.Current {
		return line + ": CURRENT"
	}
	return line + ": not current, " + st.Why
}

const memberHelp = `usage: borg member <key> [-org KEY]

Whether key is a current member of the organisation (spec section 13): the
member class from every host, each grant verified and its inclusion path
checked against the roll of the token it names (falling back to the roll
this home folded), then judged: in the roll, scope grants member, not
expired, revealed unless opaque, and its revocation outpoint unspent per
the node. When key is this home's own, each grant is kept whole in the home
(the certificate, never a summary of it) and its fields opened through the
keyring the organisation encrypted to it.

Exit 0 current; 1 answered and not current; 3 no host answered a grant, or
nothing could say whether its revocation outpoint is spent.`

func cmdMember(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("member", memberHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usage("member <key>")
	}
	k, _, err := parseKey("key", pos[0])
	if err != nil {
		return err
	}
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	ov, err := t.rd.Org(ctx, t.hall, t.org)
	if err != nil {
		if errors.Is(err, reader.ErrNoChain) {
			return incomplete("the organisation: %v", err)
		}
		return refused("the organisation: %v", err)
	}
	problem := g.reportChain("organisation chain", ov.ChainView)
	cars, refusedCars, errs := t.rd.GrantsOf(ctx, ov, k)
	for h, e := range errs {
		g.say("host %s: NOT ASKED: %s", h, firstLine(e.Error()))
	}
	for _, r := range refusedCars {
		g.say("host %s: REFUSED grant %s (%s): %v", r.Host, r.Txid, r.Reason, r.Err)
		problem = refused("a host answered a grant that does not verify; it is not shown")
	}
	var h *home
	if e, err := g.openWallet(); err == nil && bytes.Equal(e.Signer().Identity.Compressed(), k) {
		if h, err = g.openHome(); err == nil {
			defer h.close()
		}
	}
	now := time.Now()
	current, unknown := false, false
	for _, car := range cars {
		st := reader.Judge(ctx, car, ov, g.spends(), now)
		path := "inclusion path verified"
		if !ov.PathHolds(car) {
			path = "no inclusion path verified; checked against the roll folded here"
		}
		fmt.Fprintf(g.stdout, "%s (%s)\n", grantLine(st), path)
		current = current || st.Current
		unknown = unknown || st.Unknown
		if h != nil {
			vals, err := car.Carrier.Grant.OpenFields(ctx, h.e.Signer())
			if err != nil {
				g.say("grant %s: its keyring does not open with this key: %v", car.Txid, err)
				continue
			}
			fmt.Fprintf(g.stdout, "fields  role %s scope %s until %s (opened through the keyring)\n", termsafe.Abbrev(vals["role"]), termsafe.Abbrev(vals["scope"]), termsafe.Text(vals["expiry"]))
			h.st.Hold(state.HeldGrant{Hall: t.hall, Org: hex.EncodeToString(t.org), Txid: car.Txid, Record: hex.EncodeToString(car.Carrier.Record)})
		}
	}
	if h != nil {
		if err := h.st.Save(); err != nil {
			return err
		}
	}
	switch {
	case current:
		fmt.Fprintf(g.stdout, "member  %s is a CURRENT member\n", hex.EncodeToString(k))
		return problem
	case len(cars) == 0:
		return incomplete("no host answers a grant to %s in the roll: an empty answer proves nothing", hex.EncodeToString(k))
	case unknown:
		return incomplete("%s: no source could say its grants' revocation outpoints are unspent (config key asset)", hex.EncodeToString(k))
	}
	return refused("%s holds no current grant", hex.EncodeToString(k))
}

const memberOfHelp = `usage: borg member-of <key> [-org KEY]

The groups of the organisation whose members include key: the member-of
class from every host, then each group's chain folded here from seq 0, and
the key looked for in the members the fold reaches. A group a host names
whose fold does not include the key is reported, not listed.`

func cmdMemberOf(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("member-of", memberOfHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usage("member-of <key>")
	}
	k, _, err := parseKey("key", pos[0])
	if err != nil {
		return err
	}
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	ids, errs := t.rd.GroupsOf(ctx, t.hall, t.org, k)
	var problem error
	for h, e := range errs {
		g.say("host %s: NOT ASKED: %s", h, firstLine(e.Error()))
		problem = incomplete("a host could not be asked")
	}
	n := 0
	for _, id := range ids {
		gv, err := t.rd.Group(ctx, t.hall, t.org, id)
		if err != nil {
			g.say("group %s: %v", id, err)
			continue
		}
		if p := g.reportChain("group "+id, gv.ChainView); p != nil && problem == nil {
			problem = p
		}
		if !gv.IsMember(k) {
			g.say("group %s: a host names it, and its chain folded here does not include the key", id)
			continue
		}
		n++
		fmt.Fprintf(g.stdout, "%s  %s  seq %d epoch %d, %d member(s)\n", id, termsafe.Text(gv.Token.Name), gv.Token.Seq, gv.Token.Epoch, len(gv.Members))
	}
	g.say("%s is a member of %d group(s) of the organisation", hex.EncodeToString(k), n)
	return problem
}

// purse is the home's wallet with the pool's payer, the settlement leg and
// the header source: what history and payee settle pay and take through.
func (g *global) purse(ctx context.Context, h *home, hc chaintracker.ChainTracker, maxPay uint64) (*purse.Purse, error) {
	_, asset, err := g.node(false)
	if err != nil {
		return nil, err
	}
	settler, _, err := g.settler()
	if err != nil {
		return nil, err
	}
	tip, err := asset.BestHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("node tip: %w", err)
	}
	engine.CollectChange(ctx, h.e.Pool, asset)
	if h.st.KeepPayments(h.e.Pool.UnprovenTxids()) {
		if err := h.st.Save(); err != nil {
			return nil, err
		}
	}
	s := h.e.Signer()
	// The change of this home's own unmined payment is spendable by its
	// next payment, which carries that payment in its BEEF: a payer with
	// one coin is not held to one priced question per block.
	kept := &producer.Kept{Load: func(txid string) (*transaction.Transaction, error) {
		if p := h.st.PaymentOf(txid); p != nil {
			return funding.Rebuild("", "", p.Beef)
		}
		return nil, fmt.Errorf("transaction %s is not a payment this home keeps", txid)
	}}
	allow := func() []string {
		var ids []string
		for _, p := range h.st.Payments {
			// A payment and the unmined payments under it: the next one
			// would carry them all.
			if tx, err := kept.Tx(p.Txid); err == nil && len(purse.Unmined(tx))+1 < limits.MaxUnminedPayments {
				ids = append(ids, p.Txid)
			}
		}
		return ids
	}
	newPayer := func() *producer.Payer {
		return &producer.Payer{Pool: h.e.Pool, Tip: tip.Height, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s},
			Kept: kept, Allow: allow, Settler: settler, Asset: asset, Fees: mint.DefaultFees, Poll: poll, Note: g.say}
	}
	return &purse.Purse{Embedded: h.e, NewPayer: newPayer, Fees: mint.DefaultFees, MaxPay: maxPay,
		Settler: settler, Asset: asset, Headers: hc, Wait: 10 * time.Minute, Poll: poll}, nil
}

// keepPayment records a payment the host accepted, so the next payment can
// spend its change before it mines, and hands it to the settlement leg:
// it is this home's own transaction, and once it mines its change is an
// ordinary coin whenever the payee settles. A leg that does not take it
// changes nothing: the payee broadcasts the same bytes.
func (g *global) keepPayment(ctx context.Context, h *home, p *purse.Purse, tx *transaction.Transaction) error {
	beef, err := funding.BEEF(tx)
	if err != nil {
		return err
	}
	h.st.Payments = append(h.st.Payments, state.Payment{Txid: tx.TxID().String(), Beef: hex.EncodeToString(beef)})
	if err := h.st.Save(); err != nil {
		return err
	}
	if p.Settler == nil {
		return nil
	}
	if err := p.Settler.Submit(ctx, tx); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		g.say("payment %s: not broadcast from here (%s); the payee broadcasts it when it settles", reader.Short(tx.TxID().String()), firstLine(err.Error()))
	}
	return nil
}

// limitWatch is the HTTP transport of a history question. A host gives
// each BRC-104 session a budget of signed responses and refuses a request
// over it 429 without a signature, which the SDK's client reports only as a
// failed authentication; the transport sees the status and the wait the
// host named.
type limitWatch struct {
	next http.RoundTripper
	mu   sync.Mutex
	hit  bool
	wait time.Duration
}

// The wait a host names is taken, within these bounds.
const (
	minLimitWait = time.Second
	maxLimitWait = 30 * time.Second
)

func (w *limitWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := w.next.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("x-bsv-auth-version") != "" || strings.HasSuffix(r.URL.Path, "/.well-known/auth") {
		return resp, err
	}
	wait := minLimitWait
	if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
		wait = min(time.Duration(s)*time.Second, maxLimitWait)
	}
	w.mu.Lock()
	w.hit, w.wait = true, wait
	w.mu.Unlock()
	return resp, err
}

// taken reports a refusal seen since the last call, and the wait named.
func (w *limitWatch) taken() (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	hit := w.hit
	w.hit = false
	return w.wait, hit
}

// errLimited is a request refused for its session's budget twice.
var errLimited = errors.New("the host limits how fast one session is answered")

// askLimited asks through af and, when the host refused the request for
// its session's budget, waits as the host said and asks once more. Nothing
// was signed and no payment was taken by a refused request, so what the
// purse made for it is refunded before the second.
func (g *global) askLimited(ctx context.Context, p *purse.Purse, watch *limitWatch, ask func() (*http.Response, error)) (*http.Response, error) {
	resp, err := ask()
	wait, limited := watch.taken()
	if err == nil || !limited {
		return resp, err
	}
	p.Refund()
	g.say("the host limits how fast one session is answered; waiting %s and asking once more", wait)
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, err = ask()
	if _, again := watch.taken(); err != nil && again {
		return nil, errLimited
	}
	return resp, err
}

const historyHelp = `usage: borg history [-org KEY] [-chain org|GROUP] [-after SEQ] [-at URL] [-max-sats N]

Ask a host the priced history question (spec section 10.3): every token of
a chain of the organisation (its own, or a group's), one page. It is asked
at the host's terms route (-at, default the configured history_host) over
BRC-104 authentication; the host answers 402 with its price, and the
question is asked again with a BRC-29 payment to the host from this home's
pool (BRC-105) as output 0, which the host records and its payee settles.
The payment's change is spendable by this home's next payment at once,
which then carries this one, up to 16 unmined in a row; for anything else
it is spendable once the payment mines. A price over -max-sats is not paid. Every token answered is
verified against this home's headers. The free org-seq and group-seq
classes give the same tokens one at a time.`

func cmdHistory(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("history", historyHelp)
	at := fs.String("at", "", "the host's terms route base URL (default history_host)")
	chain := fs.String("chain", "org", "org, or a group id")
	after := fs.String("after", "", "the page after this seq")
	maxSats := fs.Uint64("max-sats", limits.DefaultMaxPrice, "the most one question is paid")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("history takes no arguments")
	}
	base := *at
	if base == "" {
		base = g.cfg.HistoryHost
	}
	if base == "" {
		return usage("no host to ask: set history_host (or -at) to the host's terms route base URL")
	}
	if err := checkOrigin(base); err != nil {
		return err
	}
	if *chain != "org" {
		if _, err := reader.ParseGroup(*chain); err != nil {
			return usage("-chain: %v", err)
		}
	}
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	q := map[string]string{"hall": t.hall, "history": hex.EncodeToString(t.org), "chain": *chain}
	if *after != "" {
		q["after"] = *after
	}
	raw, _ := json.Marshal(q)
	if _, err := borgrec.ParseQuery(raw); err != nil {
		return usage("the question breaks the class rules (-after is a seq): %v", err)
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	p, err := g.purse(ctx, h, t.rd.Headers, *maxSats)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(lookup.Question{Service: borgrec.LookupService, Query: q})
	watch := &limitWatch{next: http.DefaultTransport}
	af := clients.New(p, clients.WithoutLogging(), clients.WithHttpClient(&http.Client{Timeout: g.cfg.Timeout, Transport: watch}))
	resp, err := g.askLimited(ctx, p, watch, func() (*http.Response, error) {
		return af.Fetch(ctx, strings.TrimRight(base, "/")+"/lookup", &clients.SimplifiedFetchRequestOptions{
			Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: body})
	})
	if err != nil {
		p.Refund()
		if errors.Is(err, errLimited) {
			return incomplete("history at %s: %v (429) twice; ask again later", base, err)
		}
		return fmt.Errorf("history at %s: %w", base, payWords(err, *maxSats))
	}
	defer resp.Body.Close()
	ans, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		p.Refund()
		return err
	}
	if resp.StatusCode != http.StatusOK {
		p.Refund()
		return fmt.Errorf("history at %s: status %d: %s", base, resp.StatusCode, termsafe.Abbrev(firstLine(string(ans))))
	}
	if paid := resp.Header.Get("x-bsv-payment-satoshis-paid"); paid != "" {
		if tx := p.Settle(); tx != nil {
			fmt.Fprintf(g.stdout, "paid %s sat to %s in %s, recorded by the host for its payee to settle\n", paid, termsafe.Abbrev(resp.Header.Get("x-bsv-auth-identity-key")), tx.TxID())
			if err := g.keepPayment(ctx, h, p, tx); err != nil {
				return err
			}
		}
	} else {
		p.Refund()
	}
	var a lookup.Answer
	if err := json.Unmarshal(ans, &a); err != nil || a.Type != lookup.TypeOutputList {
		return fmt.Errorf("history at %s: the answer is not an output-list", base)
	}
	var bad error
	for _, o := range a.Outputs {
		tok, err := reader.VerifyToken(ctx, o.Beef, t.hall, t.org, t.rd.Headers)
		if err != nil {
			g.say("REFUSED an answered token (%s): %v", borgrec.Reason(err), err)
			bad = refused("the host answered a token that does not verify; it is not shown")
			continue
		}
		line := fmt.Sprintf("seq %d  %s  height %d", tok.Seq(), tok.Txid, tok.Height)
		if gr := tok.Token.Group; gr != nil {
			line += fmt.Sprintf("  epoch %d  +%d -%d  %d member(s)", gr.Epoch, len(gr.Add), len(gr.Remove), gr.Count)
		} else {
			o := tok.Token.Org
			line += fmt.Sprintf("  admit %d revoke %d  %d grant(s)", len(o.Admit), len(o.Revoke), o.Count)
		}
		fmt.Fprintln(g.stdout, line)
	}
	g.say("%d token(s) of the %s chain", len(a.Outputs), *chain)
	return bad
}

// checkOrigin refuses a terms base that is not an origin (spec section
// 10.3): the terms document, the BRC-104 handshake and the priced /lookup
// all sit at the root of the origin.
func checkOrigin(base string) error {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return usage("%q is not an origin: the terms route is at the root of its own origin, where BRC-104 shakes hands (docs/host.md)", termsafe.Abbrev(base))
	}
	return nil
}

const termsHelp = `usage: borg terms [URL]

Fetch and print a host's terms document (spec section 10.3) from the base
URL of its ls_borg, an origin with no path (default the configured
history_host): the classes it prices and the price of one question of each.
A host that serves none charges for nothing. The document is informative:
the 402 is what a client pays against, and a free class is never paid for,
whatever a document says.`

func cmdTerms(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("terms", termsHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usage("terms [URL]")
	}
	base := g.cfg.HistoryHost
	if len(pos) == 1 {
		base = pos[0]
	}
	if base == "" {
		return usage("no host: give its ls_borg base URL, or set history_host")
	}
	if err := checkOrigin(base); err != nil {
		return err
	}
	t, err := reader.FetchTerms(ctx, termsClient(g.cfg.Timeout), base)
	if errors.Is(err, reader.ErrNoTerms) {
		fmt.Fprintf(g.stdout, "%s serves no terms document: it charges for nothing\n", base)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "service %s, terms %d\n", termsafe.Text(t.Service), t.Terms)
	for _, c := range t.Classes {
		cls := slices.IndexFunc(borgrec.Classes, func(x borgrec.Class) bool { return x.Name == c.Class })
		switch {
		case cls < 0:
			continue
		case borgrec.Classes[cls].Free:
			fmt.Fprintf(g.stdout, "%-14s FREE CLASS WITH A PRICE (%d sat): outside the specification; never paid\n", c.Class, c.Satoshis)
		default:
			fmt.Fprintf(g.stdout, "%-14s %d sat a question\n", c.Class, c.Satoshis)
		}
	}
	return nil
}
