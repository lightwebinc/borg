package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/state"
)

// dueItem is one member of a group whose removal is due, or will be.
type dueItem struct {
	group  *state.Group
	member string
	signer bool
	// due is set when the member holds no current grant now; why says
	// which way. Otherwise expiry is when its grant stops being current.
	due    bool
	why    string
	expiry time.Time
}

// latestGrant is the subject's grant with the latest expiry among those
// admitted, not revoked and not swept, and whether it has one at all.
func latestGrant(o *state.Org, subject string) (*state.Grant, time.Time) {
	var best *state.Grant
	var at time.Time
	for i := range o.Grants {
		gr := &o.Grants[i]
		if gr.Subject != subject || !gr.Admitted || gr.Revoked || gr.Swept != "" || !borgrec.IsMembership(gr.Scope) {
			continue
		}
		if exp, err := borgrec.ParseExpiry(gr.Expiry); err == nil && (best == nil || exp.After(at)) {
			best, at = gr, exp
		}
	}
	return best, at
}

// dueOf lists, group by group, the members whose grant is no longer
// current at now (a removal is due: rotation debt) and those whose grant
// stops being current within the window.
func dueOf(o *state.Org, orgHex string, now time.Time, within time.Duration) ([]dueItem, error) {
	signer := signerHex(orgHex)
	var out []dueItem
	for i := range o.Groups {
		gr := &o.Groups[i]
		members, _, err := engine.Members(gr)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", gr.ID, err)
		}
		for _, m := range members {
			h := hex.EncodeToString(m)
			it := dueItem{group: gr, member: h, signer: h == signer}
			g, exp := latestGrant(o, h)
			switch {
			case g == nil:
				it.due, it.why = true, "it holds no current grant (revoked, swept, or never in the roll)"
			case !now.Before(exp):
				it.due, it.why, it.expiry = true, "its grant expired "+exp.UTC().Format(borgrec.ExpiryLayout), exp
			case exp.Sub(now) <= within:
				it.expiry = exp
			default:
				continue
			}
			out = append(out, it)
		}
	}
	return out, nil
}

// printDue prints the listing and returns how many removals are due now.
func printDue(out io.Writer, items []dueItem, now time.Time) int {
	n := 0
	for _, it := range items {
		who := it.member
		if it.signer {
			who += " (the organisation's signer key: its grant is renewed by the group's next transition, not removed)"
		}
		if it.due {
			if !it.signer {
				n++
			}
			fmt.Fprintf(out, "due     group %s %s: %s: %s\n", it.group.ID, field(it.group.Name), who, it.why)
			continue
		}
		fmt.Fprintf(out, "soon    group %s %s: %s: its grant expires %s, in %s\n", it.group.ID, field(it.group.Name), who,
			it.expiry.UTC().Format(borgrec.ExpiryLayout), roundLeft(it.expiry.Sub(now)))
	}
	return n
}

const dueHelp = `usage: borg org due [-within DUR] [-apply]

List the removals due: group by group, each member whose grant is no longer
current (expired, revoked or swept), which the organization owes a removal
and a new epoch (rotation debt, spec section 7.1), and each member whose
grant stops being current within -within (default 720h). It reads this
home's own record and asks nobody. Exit 3 when a removal is due now.

-apply makes the removals due now: in each group that holds such a member,
one transition that removes them and begins a new epoch, wrapped to the
members who remain. Nothing is removed before its grant expires: run it on
a schedule (a timer, daily or hourly) and each removal follows its expiry
by at most one interval. A grant renewed before it expires (borg grant
issue to the same key) is never due.`

func (g *global) orgDue(ctx context.Context, within time.Duration, apply bool) error {
	if within < 0 {
		return usage("-within is a duration of 0 or more")
	}
	now := time.Now()
	if !apply {
		e, err := g.openWallet()
		if err != nil {
			return err
		}
		st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
		if err != nil {
			return err
		}
		if st.Org == nil {
			return usage("this home runs no organisation (borg org create)")
		}
		items, err := dueOf(st.Org, st.Identity, now, within)
		if err != nil {
			return err
		}
		n := printDue(g.stdout, items, now)
		g.say("%d removal(s) due now, %d grant(s) of group members expiring within %s", n, len(items)-countDue(items), within)
		if n > 0 {
			return incomplete("%d removal(s) due: borg org due -apply makes them, each with a new epoch", n)
		}
		return nil
	}
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	o := r.h.st.Org
	items, err := dueOf(o, r.h.st.Identity, now, 0)
	if err != nil {
		return err
	}
	groups := map[string]bool{}
	var order []string
	for _, it := range items {
		if it.due && !it.signer && !groups[it.group.ID] {
			groups[it.group.ID] = true
			order = append(order, it.group.ID)
		}
	}
	for _, id := range order {
		res, err := r.eng.GroupChange(ctx, id, engine.Change{What: "group " + id + " (removals due)"})
		if err != nil {
			return lost(err)
		}
		g.printChange(o.Group(id), res)
	}
	fmt.Fprintf(g.stdout, "applied %d group(s) with removals due\n", len(order))
	return nil
}

// roundLeft words a time left: to the second under an hour, else to the
// minute.
func roundLeft(d time.Duration) time.Duration {
	if d < time.Hour {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}

func countDue(items []dueItem) int {
	n := 0
	for _, it := range items {
		if it.due {
			n++
		}
	}
	return n
}

// dueWindow is the default window of borg org due and of doctor's note.
const dueWindow = 30 * 24 * time.Hour
