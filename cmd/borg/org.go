package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/borg/internal/borgrec"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/reader"
	"github.com/lightwebinc/borg/internal/state"
)

// grantFlags are the flags that shape a grant.
type grantFlags struct {
	role, scope, expiry *string
	admin               *bool
}

func addGrantFlags(fs interface {
	String(name, value, usage string) *string
	Bool(name string, value bool, usage string) *bool
}) grantFlags {
	return grantFlags{
		role:   fs.String("role", "member", "the grant's role: a label a client shows, 1 to 64 bytes"),
		scope:  fs.String("scope", "member", "the grant's scope (spec section 5.3); member is what makes a membership"),
		expiry: fs.String("expiry", "", "when the grant stops being current: a duration from now (8760h) or YYYY-MM-DDTHH:MM:SSZ (default one year)"),
		admin:  fs.Bool("admin", false, "add admin to the scope (shown by clients; authorises nothing on chain)"),
	}
}

// req is the grant the flags describe for subject.
func (f grantFlags) req(subject *ec.PublicKey, now time.Time) (engine.GrantReq, error) {
	r := engine.GrantReq{Subject: subject, Role: *f.role, Scope: *f.scope, Expiry: now.Add(limits.DefaultExpiry)}
	if *f.admin && !strings.Contains(","+r.Scope+",", ",admin,") {
		r.Scope += ",admin"
	}
	if s := *f.expiry; s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			r.Expiry = now.Add(d)
		} else if t, err := borgrec.ParseExpiry(s); err == nil {
			r.Expiry = t
		} else {
			return r, usage("-expiry %q is neither a positive duration nor YYYY-MM-DDTHH:MM:SSZ", termsafe.Abbrev(s))
		}
	}
	if err := borgrec.CheckRole(r.Role); err != nil {
		return r, usage("-role %q: 1 to 64 bytes of text", termsafe.Abbrev(r.Role))
	}
	if _, err := borgrec.ParseScope(r.Scope); err != nil {
		return r, usage("-scope %q breaks the scope grammar (spec section 5.3): %v", termsafe.Abbrev(r.Scope), err)
	}
	if !borgrec.IsMembership(r.Scope) {
		return r, usage("-scope %q does not grant member: such a grant is not a membership", termsafe.Abbrev(r.Scope))
	}
	return r, nil
}

// orgRun is an open home with a publisher, for the organisation's
// commands.
type orgRun struct {
	h   *home
	eng *engine.Engine
	key *ec.PrivateKey
}

// openOrg opens the home and a publisher, finishing whatever a previous
// run left. need says whether the organisation must exist already.
func (g *global) openOrg(ctx context.Context, need bool) (*orgRun, error) {
	h, err := g.openHome()
	if err != nil {
		return nil, err
	}
	o := h.st.Org
	if need && (o == nil || len(o.Tokens) == 0) {
		h.close()
		return nil, usage("this home runs no organisation yet (borg org create)")
	}
	if o != nil && g.cfg.Hall != "" && g.cfg.Hall != o.Hall {
		h.close()
		return nil, usage("the organisation lives in hall %s, and hall is set to %s", o.Hall, g.cfg.Hall)
	}
	if o != nil && g.cfg.Hall == "" {
		g.cfg.Hall = o.Hall
	}
	key, err := g.orgKey()
	if err != nil {
		h.close()
		return nil, err
	}
	eng, err := g.engine(ctx, h)
	if err != nil {
		if eng != nil {
			_ = eng.Close(context.WithoutCancel(ctx))
		}
		h.close()
		return nil, lost(err)
	}
	return &orgRun{h: h, eng: eng, key: key}, nil
}

func (r *orgRun) close(ctx context.Context, g *global) {
	_ = r.eng.Close(context.WithoutCancel(ctx))
	g.tallies(r.eng)
	r.h.close()
}

// groupArg resolves a group the organisation runs, by id or by name.
func groupArg(o *state.Org, s string) (*state.Group, error) {
	if gr := o.Group(s); gr != nil {
		return gr, nil
	}
	var found *state.Group
	for i := range o.Groups {
		if o.Groups[i].Name == s {
			if found != nil {
				return nil, usage("two groups are named %q; name it by its id", termsafe.Abbrev(s))
			}
			found = &o.Groups[i]
		}
	}
	if found == nil {
		return nil, usage("group %q is not one of this organisation's (borg group list)", termsafe.Abbrev(s))
	}
	return found, nil
}

// lost words a transition another one beat to the chain.
func lost(err error) error {
	var le *engine.LostError
	if errors.As(err, &le) {
		return refused("%v", err)
	}
	return err
}

const orgHelp = `usage: borg org create <name> [-opaque] [-member KEY]... [grant flags]
       borg org show
       borg org add <key>... -group GROUP [grant flags]
       borg org remove <key>... [-group GROUP]
       borg org rekey <old> <new> [grant flags]
       borg org signer
       borg org sync [-retire-trees]
       borg org due [-within DUR] [-apply]

The home's identity key is the organisation. Every change is a mined token
of the organisation's chain or of a group's, built here, persisted with
any epoch key it samples, settled, waited for until it mines in this
home's headers, and published (after the propagation delay) to the plane
and to every host named, or in unicast to every host; then confirmed at
each host by a lookup.

create mints a chain tree and a grant tree, issues a grant to each -member,
publishes them, and mints the organisation's first token admitting them.
-opaque publishes grants without their fields in the clear: hosts and
readers then see that a grant exists, not its role or scope. It is fixed
at create.

add issues a grant to each key that holds no current grant, admits them in
one organisation token, and adds every key to the group in one group
token. A key the group removed earlier comes back only with a new epoch.

remove with -group removes the keys from the group: a new epoch, its key
wrapped to every member who remains. Without -group it offboards them:
every grant revoked in the roll and by a sweep of its revocation outpoint,
then a removal, with a new epoch, from every group that holds them.

rekey is a member's key change: a grant to the new key, the old key's
grants revoked (roll and sweep), and in every group a transition that
removes the old key and adds the new one, which begins a new epoch.

signer grants the organisation's signer key and adds it to every group
that lacks it. The signer key is derived from the organisation's identity
key, so every home made around that key derives it; as a member, it is
wrapped each epoch's key like any other, and that wrap is what org sync
opens. With signers above one every group this home creates or changes
includes it without being asked; org signer does it now, or for an
organisation run from one machine that wants a second home to be able to
catch up.

sync rebuilds this home's record of the organisation from the hosts, as a
reader reads it: the organisation's chain and each group's, verified
against this home's headers and folded from seq 0; every grant of the
roll, whose root must be reproduced; and each epoch key, opened from the
wrap to the signer key and checked against the epoch's commitment. It is
what a home runs after another instance of the tooling changed something
(with signers above one a command does it by itself), and once on a new
machine (borg init -from). It cannot bring back the key of an epoch the
signer key was not a member of, a grant issued and not yet admitted, or
another home's funding trees and unpublished work; each epoch it cannot
open is named. -retire-trees stops this home spending its current grant
and record trees: use it once on a home that was copied from another.

due lists the removals owed (members of a group whose grant is no longer
current) and the grants of group members that expire within -within;
-apply makes the removals due now, each with a new epoch (borg org due -h).

Every group transition first removes the group's rotation debt (members
whose grant is no longer current), and no transition is built that a host
would refuse or an index would report.`

func cmdOrg(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("org", orgHelp)
	gf := addGrantFlags(fs)
	opaque := fs.Bool("opaque", false, "create: publish grants without their fields in the clear")
	group := fs.String("group", "", "add, remove: the group")
	within := fs.Duration("within", dueWindow, "due: also list group members whose grant expires within this long")
	apply := fs.Bool("apply", false, "due: make the removals due now, each with a new epoch")
	retire := fs.Bool("retire-trees", false, "sync: stop spending this home's current grant and record trees (a home copied from another)")
	var members listFlag
	fs.Var(&members, "member", "create: a first member's identity key; repeatable")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usage("org create <name> | org show | org add <key>... -group G | org remove <key>... [-group G] | org rekey <old> <new> | org signer | org sync | org due")
	}
	switch pos[0] {
	case "create":
		if len(pos) != 2 {
			return usage("org create <name>")
		}
		return g.orgCreate(ctx, pos[1], *opaque, members, gf)
	case "show":
		return g.orgShow()
	case "add":
		if len(pos) < 2 || *group == "" {
			return usage("org add <key>... -group GROUP")
		}
		return g.orgAdd(ctx, pos[1:], *group, gf)
	case "remove":
		if len(pos) < 2 {
			return usage("org remove <key>... [-group GROUP]")
		}
		return g.orgRemove(ctx, pos[1:], *group)
	case "rekey":
		if len(pos) != 3 {
			return usage("org rekey <old> <new>")
		}
		return g.orgRekey(ctx, pos[1], pos[2], gf)
	case "signer":
		if len(pos) != 1 {
			return usage("org signer")
		}
		return g.orgSigner(ctx)
	case "due":
		if len(pos) != 1 {
			return usage("org due [-within DUR] [-apply]")
		}
		return g.orgDue(ctx, *within, *apply)
	case "sync":
		if len(pos) != 1 {
			return usage("org sync [-retire-trees]")
		}
		return g.orgSync(ctx, *retire)
	}
	return usage("org %q: create, show, add, remove, rekey, signer, sync or due", termsafe.Abbrev(pos[0]))
}

func (g *global) orgCreate(ctx context.Context, name string, opaque bool, members []string, gf grantFlags) error {
	if l := len(name); l < 1 || l > borgrec.MaxName {
		return usage("the organisation's name is 1 to %d bytes", borgrec.MaxName)
	}
	hall, err := g.hall()
	if err != nil {
		return err
	}
	now := time.Now()
	var reqs []engine.GrantReq
	seen := map[string]bool{}
	for _, m := range members {
		_, pub, err := parseKey("-member", m)
		if err != nil {
			return err
		}
		if seen[m] {
			return usage("-member %s is named twice", m)
		}
		seen[m] = true
		r, err := gf.req(pub, now)
		if err != nil {
			return err
		}
		reqs = append(reqs, r)
	}
	r, err := g.openOrg(ctx, false)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	switch o := r.h.st.Org; {
	case o == nil || len(o.Tokens) == 0:
		if err := r.eng.CreateOrg(ctx, r.key, hall, name, opaque, reqs); err != nil {
			return lost(err)
		}
	case len(o.Tokens) == 1 && o.Name == name && o.Hall == hall:
		// A previous run's first token, finished as this one opened.
		g.say("the organisation's first token was persisted by a previous run and is finished now")
	default:
		return usage("this home runs the organisation %q already (borg org show)", termsafe.Text(o.Name))
	}
	o := r.h.st.Org
	t := state.Latest(o.Tokens)
	fmt.Fprintf(g.stdout, "org     %s\nhall    %s\nname    %s\nseq     0 %s\n", r.eng.Signer.IdentityHex(), o.Hall, termsafe.Text(o.Name), t.Txid)
	for _, gr := range o.Grants {
		fmt.Fprintf(g.stdout, "grant   %s %s %s\n", gr.Txid, gr.Subject, termsafe.Text(gr.Role))
	}
	return nil
}

func (g *global) orgShow() error {
	e, err := g.openWallet()
	if err != nil {
		return err
	}
	st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
	if err != nil {
		return err
	}
	o := st.Org
	if o == nil {
		return usage("this home runs no organisation (borg org create)")
	}
	fmt.Fprintf(g.stdout, "org     %s\nhall    %s\nname    %s\nopaque  %v\n", st.Identity, o.Hall, termsafe.Text(o.Name), o.Opaque)
	for _, t := range o.Tokens {
		fmt.Fprintf(g.stdout, "seq     %d %s height %d\n", t.Seq, t.Txid, t.Height)
	}
	if p := o.Pending; p != nil {
		fmt.Fprintf(g.stdout, "pending %s %s (the next command finishes it)\n", termsafe.Text(p.What), p.Txid)
	}
	now := time.Now()
	signer := signerHex(st.Identity)
	for _, gr := range o.Grants {
		st := "issued"
		switch {
		case gr.Swept != "":
			st = "swept by " + gr.Swept
		case gr.Revoked:
			st = fmt.Sprintf("revoked at seq %d", gr.RevSeq)
		case gr.Admitted:
			st = "in the roll"
			if exp, err := borgrec.ParseExpiry(gr.Expiry); err == nil && !now.Before(exp) {
				st = "in the roll, EXPIRED"
			}
		}
		fmt.Fprintf(g.stdout, "grant   %s %s %s %s until %s: %s\n", gr.Txid, gr.Subject, termsafe.Text(gr.Role), termsafe.Text(gr.Scope), gr.Expiry, st)
	}
	for i := range o.Groups {
		gr := &o.Groups[i]
		members, _, err := engine.Members(gr)
		if err != nil {
			return err
		}
		t := state.Latest(gr.Tokens)
		seq := uint64(0)
		if t != nil {
			seq = t.Seq
		}
		ep, held := currentEpoch(gr)
		line := fmt.Sprintf("group   %s %s seq %d epoch %d, %d member(s)", gr.ID, termsafe.Text(gr.Name), seq, ep, len(members))
		if !held {
			line += "; KEY NOT HELD: the epoch was begun by another instance (borg org sync opens it)"
		}
		fmt.Fprintln(g.stdout, line)
		for _, m := range members {
			mark := ""
			if hex.EncodeToString(m) == signer {
				mark = "  (the organisation's signer key)"
			}
			if currentAt(o, hex.EncodeToString(m), now) == nil {
				mark += "  ROTATION DEBT: no current grant"
			}
			fmt.Fprintf(g.stdout, "member  %s%s\n", hex.EncodeToString(m), mark)
		}
	}
	return nil
}

// signerHex is the signer key of the organisation orgHex, hex, or "".
func signerHex(orgHex string) string {
	org, err := hex.DecodeString(orgHex)
	if err != nil {
		return ""
	}
	sk, err := borgrec.SignerKey(org)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(sk)
}

// currentEpoch is the epoch a group's latest token is in, and whether this
// home holds its key.
func currentEpoch(gr *state.Group) (uint32, bool) {
	recs, err := engine.GroupRecords(gr)
	if err != nil || len(recs) == 0 {
		return 0, false
	}
	ep := uint32(recs[len(recs)-1].Epoch) //nolint:gosec // at most 2^32 - 1
	return ep, gr.EpochOf(ep) != nil
}

// syncFailed words a catch-up that could not be made, with its exit code.
func syncFailed(err error) error {
	var se *engine.SyncError
	if errors.As(err, &se) {
		if se.Incomplete {
			return incomplete("catching up: %s", se.Msg)
		}
		return refused("catching up: %s", se.Msg)
	}
	return err
}

// reportSync prints what a catch-up did.
func (g *global) reportSync(out io.Writer, rep *engine.SyncReport) {
	for _, h := range rep.Unasked {
		fmt.Fprintf(out, "sync    host %s could not be asked; the chain is read from the others\n", h)
	}
	if !rep.Changed() && len(rep.Missing) == 0 {
		fmt.Fprintln(out, "sync    nothing to catch up: this home holds what the hosts answer")
		return
	}
	if rep.Displaced > 0 {
		fmt.Fprintf(out, "sync    %d token(s) of this home's are no longer in the chain and were dropped\n", rep.Displaced)
	}
	fmt.Fprintf(out, "sync    took %d organisation token(s), %d group token(s) (%d new group(s)), %d grant(s), %d epoch key(s)\n",
		rep.OrgTokens, rep.GroupTokens, rep.Groups, rep.Grants, rep.Keys)
	for _, m := range rep.Missing {
		cur := ""
		if m.Current {
			cur = ", the CURRENT epoch: this home wraps it to nobody until it opens it or the group rotates"
		}
		fmt.Fprintf(out, "missing group %s epoch %d%s: %s\n", m.Group, m.Epoch, cur, termsafe.Text(m.Why))
	}
	if rep.Coins+rep.Spent > 0 {
		fmt.Fprintf(out, "sync    pool: %d coin(s) added (change of the tokens taken), %d removed (spent by another instance)\n", rep.Coins, rep.Spent)
	}
	for _, t := range rep.Retired {
		fmt.Fprintf(out, "sync    funding tree %s is no longer spent from this home: another instance may spend it too\n", t)
	}
}

// orgSync catches the home up with the hosts.
func (g *global) orgSync(ctx context.Context, retire bool) error {
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	if o := h.st.Org; o != nil {
		if g.cfg.Hall != "" && g.cfg.Hall != o.Hall {
			return usage("the organisation lives in hall %s, and hall is set to %s", o.Hall, g.cfg.Hall)
		}
		g.cfg.Hall = o.Hall
	}
	hall, err := g.hall()
	if err != nil {
		return err
	}
	g.explicitSync = true
	eng, err := g.engine(ctx, h)
	if eng != nil {
		defer func() {
			_ = eng.Close(context.WithoutCancel(ctx))
			g.tallies(eng)
		}()
	}
	var lostErr *engine.LostError
	if errors.As(err, &lostErr) {
		g.say("%v", err)
	} else if err != nil {
		return err
	}
	rep, err := eng.Sync(ctx, hall, engine.SyncOptions{RetireTrees: retire})
	if err != nil {
		return syncFailed(err)
	}
	g.reportSync(g.stdout, rep)
	o := h.st.Org
	fmt.Fprintf(g.stdout, "org     %s %s in %s: seq %d, %d grant(s), %d group(s)\n", h.st.Identity, termsafe.Text(o.Name), o.Hall, len(o.Tokens)-1, len(o.Grants), len(o.Groups))
	// What the home owes now that it holds the chain: a wrap to a member
	// another home added, when this one holds the epoch's key. A home with
	// no coin yet makes them with its next command.
	if err := eng.Owed(ctx); err != nil {
		g.say("the wraps this home owes were not all made: %s; the next command makes them", firstLine(err.Error()))
	}
	for _, m := range rep.Missing {
		if m.Current {
			return incomplete("group %s: the key of the current epoch %d is not open here; ask again after the next block (another instance publishes its wraps one block after its token), run `borg org signer` where the key is held, or rotate the group", m.Group, m.Epoch)
		}
	}
	return nil
}

// orgSigner grants the signer key and adds it to every group that lacks
// it.
func (g *global) orgSigner(ctx context.Context) error {
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	sk, err := r.eng.EnsureSigner(ctx)
	if err != nil {
		return lost(err)
	}
	fmt.Fprintf(g.stdout, "signer  %s\n", hex.EncodeToString(sk))
	o := r.h.st.Org
	for i := range o.Groups {
		gr := &o.Groups[i]
		members, _, err := engine.Members(gr)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(members, func(m []byte) bool { return string(m) == string(sk) }) {
			fmt.Fprintf(g.stdout, "group   %s %s: a member already\n", gr.ID, termsafe.Text(gr.Name))
			continue
		}
		res, err := r.eng.GroupChange(ctx, gr.ID, engine.Change{What: "group " + gr.ID + " (add the signer key)"})
		if err != nil {
			return lost(err)
		}
		g.printChange(gr, res)
	}
	return nil
}

// admitNew issues a grant to every key that holds no current grant and
// admits them in one organisation token.
func (g *global) admitNew(ctx context.Context, r *orgRun, keys []*ec.PublicKey, gf grantFlags) error {
	var issued []*state.Grant
	now := time.Now()
	for _, k := range keys {
		hexk := hex.EncodeToString(k.Compressed())
		if r.eng.CurrentGrant(hexk) != nil {
			continue
		}
		var pendingGrant *state.Grant
		for i := range r.h.st.Org.Grants {
			og := &r.h.st.Org.Grants[i]
			if og.Subject == hexk && !og.Admitted && !og.Revoked && og.Swept == "" {
				pendingGrant = og
			}
		}
		if pendingGrant == nil {
			req, err := gf.req(k, now)
			if err != nil {
				return err
			}
			gr, err := r.eng.IssueGrant(ctx, r.key, req)
			if err != nil {
				return err
			}
			pendingGrant = gr
			fmt.Fprintf(g.stdout, "grant   %s to %s\n", gr.Txid, hexk)
		}
		issued = append(issued, pendingGrant)
	}
	if len(issued) == 0 {
		return nil
	}
	if err := r.eng.OrgTransition(ctx, issued, nil, "organisation token (admit)"); err != nil {
		return lost(err)
	}
	t := state.Latest(r.h.st.Org.Tokens)
	fmt.Fprintf(g.stdout, "org     seq %d %s admits %d grant(s)\n", t.Seq, t.Txid, len(issued))
	return nil
}

func (g *global) printChange(gr *state.Group, res *engine.ChangeResult) {
	t := state.Latest(gr.Tokens)
	line := fmt.Sprintf("group   %s seq %d %s epoch %d", gr.ID, t.Seq, t.Txid, res.Epoch)
	if res.Rotated {
		line += " (a new epoch: a fresh key, wrapped to every member)"
	}
	fmt.Fprintln(g.stdout, line)
	for _, d := range res.Debt {
		fmt.Fprintf(g.stdout, "removed %s: rotation debt (no current grant)\n", hex.EncodeToString(d))
	}
	for _, k := range res.Rejoin {
		fmt.Fprintf(g.stdout, "rejoin  %s: removed earlier, so the epoch rotated; it reads from this epoch on\n", hex.EncodeToString(k))
	}
}

func (g *global) orgAdd(ctx context.Context, keys []string, group string, gf grantFlags) error {
	var pubs []*ec.PublicKey
	var raw [][]byte
	for _, s := range keys {
		b, pub, err := parseKey("key", s)
		if err != nil {
			return err
		}
		pubs, raw = append(pubs, pub), append(raw, b)
	}
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	gr, err := groupArg(r.h.st.Org, group)
	if err != nil {
		return err
	}
	if err := g.admitNew(ctx, r, pubs, gf); err != nil {
		return err
	}
	res, err := r.eng.GroupChange(ctx, gr.ID, engine.Change{Add: raw, What: "group " + gr.ID + " (add)"})
	if err != nil {
		return lost(err)
	}
	g.printChange(r.h.st.Org.Group(gr.ID), res)
	return nil
}

// grantsOf are the subject's grants not yet revoked in the roll or swept.
func grantsOf(o *state.Org, subject string) []*state.Grant {
	var out []*state.Grant
	for i := range o.Grants {
		gr := &o.Grants[i]
		if gr.Subject == subject && (!gr.Revoked || gr.Swept == "") {
			out = append(out, gr)
		}
	}
	return out
}

// revoke revokes grants by the roll (one organisation token) and by sweep
// (one sweep per grant tree), as by says.
func (g *global) revoke(ctx context.Context, r *orgRun, grants []*state.Grant, by string) error {
	o := r.h.st.Org
	if by == "both" || by == "roll" {
		var inRoll []*state.Grant
		for _, gr := range grants {
			if gr.Admitted && !gr.Revoked {
				inRoll = append(inRoll, gr)
			}
		}
		if len(inRoll) > 0 {
			if err := r.eng.OrgTransition(ctx, nil, inRoll, "organisation token (revoke)"); err != nil {
				return lost(err)
			}
			t := state.Latest(o.Tokens)
			fmt.Fprintf(g.stdout, "org     seq %d %s revokes %d grant(s) in the roll\n", t.Seq, t.Txid, len(inRoll))
		}
	}
	if by == "both" || by == "sweep" {
		trees := map[string][]uint32{}
		var order []string
		for _, gr := range grants {
			if gr.Swept != "" {
				continue
			}
			if _, ok := trees[gr.Tree]; !ok {
				order = append(order, gr.Tree)
			}
			trees[gr.Tree] = append(trees[gr.Tree], gr.Vout)
		}
		for _, tree := range order {
			sw, err := r.eng.Retract(ctx, o.Hall, tree, trees[tree])
			if err != nil {
				return err
			}
			fmt.Fprintf(g.stdout, "sweep   %s spends %d revocation outpoint(s) of %s, mined at height %d\n", sw.Txid, len(sw.Vouts), tree, sw.Height)
		}
		if len(order) > 0 {
			g.say("a sweep revokes for every BRC-52 verifier and retracts at every honest host; it is not erasure")
		}
	}
	return nil
}

func (g *global) orgRemove(ctx context.Context, keys []string, group string) error {
	var raw [][]byte
	for _, s := range keys {
		b, _, err := parseKey("key", s)
		if err != nil {
			return err
		}
		raw = append(raw, b)
	}
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	o := r.h.st.Org
	if group != "" {
		gr, err := groupArg(o, group)
		if err != nil {
			return err
		}
		res, err := r.eng.GroupChange(ctx, gr.ID, engine.Change{Remove: raw, What: "group " + gr.ID + " (remove)"})
		if err != nil {
			return lost(err)
		}
		g.printChange(o.Group(gr.ID), res)
		return nil
	}
	// Offboard: every grant revoked, by the roll and by sweep, then a
	// removal from every group that holds the keys.
	var grants []*state.Grant
	for _, k := range keys {
		grants = append(grants, grantsOf(o, k)...)
	}
	if err := g.revoke(ctx, r, grants, "both"); err != nil {
		return err
	}
	for i := range o.Groups {
		gr := &o.Groups[i]
		members, _, err := engine.Members(gr)
		if err != nil {
			return err
		}
		var here [][]byte
		for _, k := range raw {
			if slices.ContainsFunc(members, func(m []byte) bool { return string(m) == string(k) }) {
				here = append(here, k)
			}
		}
		if len(here) == 0 {
			continue
		}
		res, err := r.eng.GroupChange(ctx, gr.ID, engine.Change{Remove: here, What: "group " + gr.ID + " (offboard)"})
		if err != nil {
			return lost(err)
		}
		g.printChange(gr, res)
	}
	fmt.Fprintf(g.stdout, "offboarded %d key(s)\n", len(raw))
	return nil
}

func (g *global) orgRekey(ctx context.Context, oldS, newS string, gf grantFlags) error {
	oldK, _, err := parseKey("old", oldS)
	if err != nil {
		return err
	}
	newK, newPub, err := parseKey("new", newS)
	if err != nil {
		return err
	}
	if oldS == newS {
		return usage("the old and new keys are the same")
	}
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	o := r.h.st.Org
	cur := r.eng.CurrentGrant(oldS)
	if cur == nil {
		return refused("%s holds no current grant; a key change moves a current membership", oldS)
	}
	// The new key's grant carries the old one's role and scope unless the
	// flags say otherwise.
	if *gf.role == "member" {
		*gf.role = cur.Role
	}
	if *gf.scope == "member" {
		*gf.scope = cur.Scope
	}
	req, err := gf.req(newPub, time.Now())
	if err != nil {
		return err
	}
	ng, err := r.eng.IssueGrant(ctx, r.key, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "grant   %s to %s\n", ng.Txid, newS)
	old := grantsOf(o, oldS)
	var inRoll []*state.Grant
	for _, gr := range old {
		if gr.Admitted && !gr.Revoked {
			inRoll = append(inRoll, gr)
		}
	}
	if err := r.eng.OrgTransition(ctx, []*state.Grant{o.GrantByTxid(ng.Txid)}, inRoll, "organisation token (key change)"); err != nil {
		return lost(err)
	}
	t := state.Latest(o.Tokens)
	fmt.Fprintf(g.stdout, "org     seq %d %s admits the new key's grant and revokes %d of the old key's\n", t.Seq, t.Txid, len(inRoll))
	if err := g.revoke(ctx, r, grantsOf(o, oldS), "sweep"); err != nil {
		return err
	}
	for i := range o.Groups {
		gr := &o.Groups[i]
		members, _, err := engine.Members(gr)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(members, func(m []byte) bool { return string(m) == string(oldK) }) {
			continue
		}
		res, err := r.eng.GroupChange(ctx, gr.ID, engine.Change{Add: [][]byte{newK}, Remove: [][]byte{oldK}, What: "group " + gr.ID + " (key change)"})
		if err != nil {
			return lost(err)
		}
		g.printChange(gr, res)
	}
	return nil
}

const grantHelp = `usage: borg grant issue <key> [-role R] [-scope S] [-admin] [-expiry E]
       borg grant revoke <key|txid>... [-by both|roll|sweep]

issue signs a BRC-52 membership grant to the key (certifier the
organisation; its revocation outpoint the grant-tree output its carrier
spends), publishes it, and admits it to the roll in the next organisation
token. The member reads it with borg member <its key>, which keeps it whole
in the member's home.

revoke revokes every grant of a key not yet revoked (or one grant by its
txid) two ways (spec section 5.6): by the roll, in the next organisation
token, which every reader of borg's records sees at once; and by a mined
sweep of its revocation outpoint, which every BRC-52 verifier sees and
which retracts the carrier at every host. -by roll or -by sweep does one.
A revoked key that is still in a group is rotation debt: the group's next
transition removes it (borg epoch rotate clears it now).`

func cmdGrant(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("grant", grantHelp)
	gf := addGrantFlags(fs)
	by := fs.String("by", "both", "revoke: both, roll or sweep")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 2 && pos[0] == "issue":
		_, pub, err := parseKey("key", pos[1])
		if err != nil {
			return err
		}
		req, err := gf.req(pub, time.Now())
		if err != nil {
			return err
		}
		r, err := g.openOrg(ctx, true)
		if err != nil {
			return err
		}
		defer r.close(ctx, g)
		gr, err := r.eng.IssueGrant(ctx, r.key, req)
		if err != nil {
			return err
		}
		fmt.Fprintf(g.stdout, "grant   %s to %s, %s, %s, until %s\n", gr.Txid, gr.Subject, termsafe.Text(gr.Role), termsafe.Text(gr.Scope), gr.Expiry)
		if err := r.eng.OrgTransition(ctx, []*state.Grant{r.h.st.Org.GrantByTxid(gr.Txid)}, nil, "organisation token (admit)"); err != nil {
			return lost(err)
		}
		t := state.Latest(r.h.st.Org.Tokens)
		fmt.Fprintf(g.stdout, "org     seq %d %s admits it\n", t.Seq, t.Txid)
		return nil
	case len(pos) >= 2 && pos[0] == "revoke":
		if *by != "both" && *by != "roll" && *by != "sweep" {
			return usage("-by must be both, roll or sweep")
		}
		r, err := g.openOrg(ctx, true)
		if err != nil {
			return err
		}
		defer r.close(ctx, g)
		o := r.h.st.Org
		var grants []*state.Grant
		subjects := map[string]bool{}
		for _, s := range pos[1:] {
			switch {
			case len(s) == 66:
				if _, _, err := parseKey("key", s); err != nil {
					return err
				}
				gs := grantsOf(o, s)
				if len(gs) == 0 {
					return usage("%s holds no grant left to revoke", s)
				}
				grants = append(grants, gs...)
				subjects[s] = true
			default:
				if _, err := reader.Hash(s); err != nil {
					return usage("%q is neither an identity key nor a grant's txid", termsafe.Abbrev(s))
				}
				gr := o.GrantByTxid(s)
				if gr == nil {
					return usage("grant %s is not one this organisation issued", s)
				}
				grants = append(grants, gr)
				subjects[gr.Subject] = true
			}
		}
		if err := g.revoke(ctx, r, grants, *by); err != nil {
			return err
		}
		for i := range o.Groups {
			members, _, _ := engine.Members(&o.Groups[i])
			for _, m := range members {
				if subjects[hex.EncodeToString(m)] && r.eng.CurrentGrant(hex.EncodeToString(m)) == nil {
					g.say("group %s: %s is rotation debt; the group's next transition removes it (borg epoch rotate %s)", o.Groups[i].ID, hex.EncodeToString(m), o.Groups[i].ID)
				}
			}
		}
		return nil
	}
	return usage("grant issue <key> | grant revoke <key|txid>...")
}

const groupHelp = `usage: borg group create <name> [-member KEY]... [-rotate-on-join] [-rotate-every DUR]
       borg group list
       borg group show <group> [-org KEY]

create draws a group id and an epoch key, mints the group's first token
(epoch 0, the members added) on a chain-tree output, and once it is mined
publishes it and wraps the epoch key to every member. Every member must
hold a current grant (borg grant issue, or borg org add). -rotate-on-join
makes every later addition begin a new epoch too; -rotate-every records the
schedule of rotations the organisation intends (no host enforces it).

list prints the organisation's groups: this home's own record, or with
-org another organisation's, read from the hosts.

show reads a group as any reader does (spec section 13): its chain from
every host, folded here from seq 0, and its members enumerated from the
wraps of the current epoch, whose recipients must be exactly the members
the latest token's root names. A wrap to a key the chain never admitted
(stray) or a member with no wrap (incomplete) is reported, and the group
is shown as inconsistent (exit 3).`

func cmdGroup(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("group", groupHelp)
	var members listFlag
	fs.Var(&members, "member", "create: a member's identity key; repeatable")
	onJoin := fs.Bool("rotate-on-join", false, "create: every addition begins a new epoch")
	every := fs.Duration("rotate-every", 0, "create: the rotation schedule the organisation intends (informative)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 2 && pos[0] == "create":
		if l := len(pos[1]); l < 1 || l > borgrec.MaxName {
			return usage("a group's name is 1 to %d bytes", borgrec.MaxName)
		}
		var raw [][]byte
		for _, m := range members {
			b, _, err := parseKey("-member", m)
			if err != nil {
				return err
			}
			raw = append(raw, b)
		}
		if *every < 0 || *every > time.Duration(borgrec.MaxEpoch)*time.Second {
			return usage("-rotate-every is 0 to %d seconds", uint64(borgrec.MaxEpoch))
		}
		r, err := g.openOrg(ctx, true)
		if err != nil {
			return err
		}
		defer r.close(ctx, g)
		gr, err := r.eng.CreateGroup(ctx, pos[1], raw, *onJoin, uint64(every.Seconds()))
		if err != nil {
			return lost(err)
		}
		t := state.Latest(gr.Tokens)
		n := len(raw)
		if members, _, err := engine.Members(gr); err == nil {
			n = len(members)
		}
		fmt.Fprintf(g.stdout, "group   %s\nname    %s\nseq     0 %s epoch 0, %d member(s), wrapped to each\n", gr.ID, termsafe.Text(gr.Name), t.Txid, n)
		return nil
	case len(pos) == 1 && pos[0] == "list":
		return g.groupList(ctx)
	case len(pos) == 2 && pos[0] == "show":
		return g.groupShow(ctx, pos[1])
	}
	return usage("group create <name> | group list | group show <group>")
}

func (g *global) groupList(ctx context.Context) error {
	e, err := g.openWallet()
	if err == nil && (g.cfg.Org == "" || g.cfg.Org == e.Signer().IdentityHex()) {
		st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
		if err != nil {
			return err
		}
		if st.Org != nil {
			for i := range st.Org.Groups {
				gr := &st.Org.Groups[i]
				members, _, _ := engine.Members(gr)
				fmt.Fprintf(g.stdout, "%s  %s  %d member(s)\n", gr.ID, termsafe.Text(gr.Name), len(members))
			}
			return nil
		}
	}
	rt, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	ids, errs := rt.rd.GroupsAll(ctx, rt.hall, rt.org)
	for h, e := range errs {
		g.say("host %s: NOT ASKED: %s", h, firstLine(e.Error()))
	}
	for _, id := range ids {
		gv, err := rt.rd.Group(ctx, rt.hall, rt.org, id)
		if err != nil {
			g.say("group %s: %v", id, err)
			continue
		}
		fmt.Fprintf(g.stdout, "%s  %s  seq %d epoch %d, %d member(s)\n", id, termsafe.Text(gv.Token.Name), gv.Token.Seq, gv.Token.Epoch, len(gv.Members))
	}
	if len(errs) > 0 {
		return incomplete("%d host(s) could not be asked", len(errs))
	}
	return nil
}

const epochHelp = `usage: borg epoch rotate <group>

Begin a new epoch with no change of members: a freshly sampled key,
committed in the group's next token and wrapped to every member once it is
mined. Use it on the group's rotateEvery schedule, after a suspected leak
of a key (rotation gives forward exclusion, not recovery), or to clear
rotation debt now: every transition first removes members whose grant is
no longer current.`

func cmdEpoch(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("epoch", epochHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || pos[0] != "rotate" {
		return usage("epoch rotate <group>")
	}
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	gr, err := groupArg(r.h.st.Org, pos[1])
	if err != nil {
		return err
	}
	res, err := r.eng.GroupChange(ctx, gr.ID, engine.Change{Rotate: true, What: "group " + gr.ID + " (rotate)"})
	if err != nil {
		return lost(err)
	}
	g.printChange(r.h.st.Org.Group(gr.ID), res)
	return nil
}

func (g *global) groupShow(ctx context.Context, name string) error {
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	gid, err := g.resolveGroup(ctx, t, name)
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
	tok := gv.Token
	fmt.Fprintf(g.stdout, "group   %s %s\nlatest  seq %d %s epoch %d (begun at seq %d), %d member(s)\n", gid, termsafe.Text(tok.Name), tok.Seq,
		gv.Latest().Txid, tok.Epoch, tok.EpochSeq, len(gv.Members))
	if tok.RotateOnJoin || tok.RotateEvery > 0 {
		fmt.Fprintf(g.stdout, "policy  rotateOnJoin %v, rotateEvery %ds (informative)\n", tok.RotateOnJoin, tok.RotateEvery)
	}
	got, refusedWraps := t.rd.Recipients(ctx, gv, gv.Epoch())
	for _, r := range refusedWraps {
		g.say("host %s: REFUSED wrap %s (%s): %v", r.Host, r.Txid, r.Reason, r.Err)
	}
	var stray, missing []string
	in := map[string]bool{}
	for _, m := range gv.Members {
		in[string(m)] = true
		mark := ""
		if !got[string(m)] {
			mark = "  INCOMPLETE: no wrap of the current epoch names it"
			missing = append(missing, hex.EncodeToString(m))
		}
		fmt.Fprintf(g.stdout, "member  %s%s\n", hex.EncodeToString(m), mark)
	}
	for r := range got {
		if !in[r] {
			stray = append(stray, hex.EncodeToString([]byte(r)))
		}
	}
	slices.Sort(stray)
	for _, s := range stray {
		fmt.Fprintf(g.stdout, "stray   %s: a wrap names a key the chain never made a member\n", s)
	}
	if len(stray) == 0 && len(missing) > 0 {
		// Wraps follow their token's block, one further block where several
		// instances can sign (spec sections 7.2 and 13): within that window
		// an incomplete release is pending, not inconsistent.
		if tip, err := t.rd.Headers.CurrentHeight(ctx); err == nil && tip <= gv.Latest().Height+1 {
			return incomplete("group %s: the wraps of epoch %d are PENDING: %d member(s) without a wrap while the latest token is at most one block deep; ask again after the next block", gid, tok.Epoch, len(missing))
		}
	}
	if len(stray) > 0 || len(missing) > 0 {
		return incomplete("group %s is inconsistent: %d stray wrap recipient(s), %d member(s) without a wrap", gid, len(stray), len(missing))
	}
	if borgrec.MembersRoot(gv.Members) != tok.Members {
		return refused("group %s: the members folded here do not reproduce the latest token's root", gid)
	}
	g.say("the wraps of epoch %d name exactly the %d member(s) the latest token's root commits to", tok.Epoch, len(gv.Members))
	return problem
}
