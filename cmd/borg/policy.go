package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/state"
)

const roleHelp = `usage: borg role define <name> -permission P [-permission P ...]
       borg role retire <name>
       borg role list

A role names the permissions it carries (service/action/resource). Define
publishes the role and the organisation's next token commits to it; retire
sweeps its funding output and drops it from the set. Giving a role to a
member or a group is borg bind.`

const bindHelp = `usage: borg bind <role> -to <group-id|key> [-only P ...] [-expiry E]
       borg unbind <txid>

A binding gives a role to one key or to a whole group: every current member
of that group holds the role. -only keeps part of the role and can never
widen it. unbind retires one binding by its carrier's txid.`

const policyHelp = `usage: borg policy [show]
       borg policy check <key> <permission>

show lists the policy set this home published. check folds the set against
what this home knows of a subject (its grants and its groups) and says
whether the permission is granted, as a service would.`

// multi collects a flag given more than once.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func cmdRole(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("role", roleHelp)
	var perms multi
	fs.Var(&perms, "permission", "a permission the role carries, given once per permission")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 2 && pos[0] == "define":
		if len(perms) == 0 {
			g.say("a role with no permission authorises nothing; a client may still show it")
		}
		return g.publishPolicy(ctx, engine.PolicyReq{Role: pos[1], Permissions: perms}, "role")
	case len(pos) == 2 && pos[0] == "retire":
		r, err := g.openOrg(ctx, true)
		if err != nil {
			return err
		}
		defer r.close(ctx, g)
		var out []*state.Policy
		for _, p := range current(r.h.st.Org) {
			if p.Role == pos[1] && p.Kind == borgrec.PolicyRole {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return usage("no definition of %q is in the policy set", pos[1])
		}
		return g.retirePolicy(ctx, r, out)
	case len(pos) <= 1 && (len(pos) == 0 || pos[0] == "list"):
		r, err := g.openOrg(ctx, false)
		if err != nil {
			return err
		}
		defer r.close(ctx, g)
		roles := map[string][]string{}
		for _, p := range current(r.h.st.Org) {
			if p.Kind == borgrec.PolicyRole {
				roles[p.Role] = p.Permissions
			}
		}
		names := make([]string, 0, len(roles))
		for n := range roles {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(g.stdout, "role    %s %s\n", n, strings.Join(roles[n], " "))
		}
		if len(names) == 0 {
			g.say("this organisation's policy set defines no role (borg role define)")
		}
		return nil
	}
	return usage(roleHelp)
}

func cmdBind(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("bind", bindHelp)
	to := fs.String("to", "", "the group identifier or identity key the role is given to")
	expiry := fs.String("expiry", "", "the time the binding stops authorising, YYYY-MM-DDTHH:MM:SSZ")
	var only multi
	fs.Var(&only, "only", "keep this permission of the role, given once per permission")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usage(bindHelp)
	}
	if *to == "" {
		return usage("-to names the group identifier or the identity key the role is given to")
	}
	raw, err := hex.DecodeString(*to)
	if err != nil || (len(raw) != borgrec.GroupIDLen && len(raw) != 33) {
		return usage("-to is a group identifier (32 hex digits) or an identity key (66)")
	}
	if *expiry != "" {
		if _, err := borgrec.ParseExpiry(*expiry); err != nil {
			return usage("-expiry is YYYY-MM-DDTHH:MM:SSZ")
		}
	}
	return g.publishPolicy(ctx, engine.PolicyReq{Role: pos[0], Permissions: only, To: raw, Expiry: *expiry}, "binding")
}

func cmdUnbind(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("unbind", bindHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usage(bindHelp)
	}
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	p := r.h.st.Org.PolicyByTxid(pos[0])
	if p == nil {
		return usage("%s is not a policy record this organisation published", pos[0])
	}
	if p.Kind != borgrec.PolicyBinding {
		return usage("%s is a role definition; retire it with borg role retire %s", pos[0], p.Role)
	}
	return g.retirePolicy(ctx, r, []*state.Policy{p})
}

func cmdPolicy(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("policy", policyHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) <= 1 && (len(pos) == 0 || pos[0] == "show"):
		r, err := g.openOrg(ctx, false)
		if err != nil {
			return err
		}
		defer r.close(ctx, g)
		set := current(r.h.st.Org)
		for _, p := range set {
			if p.Kind == borgrec.PolicyRole {
				fmt.Fprintf(g.stdout, "role    %s %s %s\n", p.Txid, p.Role, strings.Join(p.Permissions, " "))
				continue
			}
			what := "key"
			if len(p.To) == 2*borgrec.GroupIDLen {
				what = "group"
			}
			fmt.Fprintf(g.stdout, "bind    %s %s to %s %s%s%s\n", p.Txid, p.Role, what, p.To,
				suffix(" only ", strings.Join(p.Permissions, " ")), suffix(" until ", p.Expiry))
		}
		if len(set) == 0 {
			g.say("this organisation's policy set is empty (borg role define)")
		}
		return nil
	case len(pos) == 3 && pos[0] == "check":
		return g.check(ctx, pos[1], pos[2])
	}
	return usage(policyHelp)
}

func suffix(label, v string) string {
	if v == "" {
		return ""
	}
	return label + v
}

// current is the policy set as this home knows it: defined by a token and
// not retired.
func current(o *state.Org) []*state.Policy {
	var out []*state.Policy
	if o == nil {
		return out
	}
	for i := range o.Policies {
		if o.Policies[i].Defined && !o.Policies[i].Retired {
			out = append(out, &o.Policies[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Txid < out[j].Txid })
	return out
}

// publishPolicy writes one record and commits to it in one transition.
func (g *global) publishPolicy(ctx context.Context, req engine.PolicyReq, what string) error {
	r, err := g.openOrg(ctx, true)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	p, err := r.eng.PublishPolicy(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "%-7s %s %s %s\n", what, p.Txid, p.Role, strings.Join(p.Permissions, " "))
	if err := r.eng.OrgTransitionWith(ctx, nil, nil, []*state.Policy{r.h.st.Org.PolicyByTxid(p.Txid)}, nil, "organisation token (define)"); err != nil {
		return lost(err)
	}
	t := state.Latest(r.h.st.Org.Tokens)
	fmt.Fprintf(g.stdout, "org     seq %d %s defines it\n", t.Seq, t.Txid)
	return nil
}

// retirePolicy drops records from the set and sweeps their outputs: the
// committed half and the fast half of a withdrawal (spec section 9.5).
func (g *global) retirePolicy(ctx context.Context, r *orgRun, ps []*state.Policy) error {
	o := r.h.st.Org
	if err := r.eng.OrgTransitionWith(ctx, nil, nil, nil, ps, "organisation token (retire)"); err != nil {
		return lost(err)
	}
	t := state.Latest(o.Tokens)
	fmt.Fprintf(g.stdout, "org     seq %d %s retires %d policy record(s)\n", t.Seq, t.Txid, len(ps))
	trees := map[string][]uint32{}
	var order []string
	for _, p := range ps {
		if p.Swept != "" {
			continue
		}
		if _, ok := trees[p.Tree]; !ok {
			order = append(order, p.Tree)
		}
		trees[p.Tree] = append(trees[p.Tree], p.Vout)
	}
	for _, tree := range order {
		sw, err := r.eng.Retract(ctx, o.Hall, tree, trees[tree])
		if err != nil {
			return err
		}
		fmt.Fprintf(g.stdout, "sweep   %s spends %d policy outpoint(s) of %s, mined at height %d\n", sw.Txid, len(sw.Vouts), tree, sw.Height)
	}
	return nil
}

// check folds this home's policy set for a subject, as a service would.
func (g *global) check(ctx context.Context, key, permission string) error {
	raw, _, err := parseKey("key", key)
	if err != nil {
		return err
	}
	if _, err := borgrec.Asked(permission); err != nil {
		return usage("%q is not a permission a service asks: service/action/resource, and never a wildcard", permission)
	}
	r, err := g.openOrg(ctx, false)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	o := r.h.st.Org
	set, err := engine.Records(current(o))
	if err != nil {
		return err
	}
	subject := borgrec.Subject{Key: raw, Member: r.eng.CurrentGrant(hex.EncodeToString(raw)) != nil}
	if gr := r.eng.CurrentGrant(hex.EncodeToString(raw)); gr != nil && gr.Role != "" {
		subject.Roles = []string{gr.Role}
	}
	for i := range o.Groups {
		members, _, _ := engine.Members(&o.Groups[i])
		for _, m := range members {
			if hex.EncodeToString(m) == hex.EncodeToString(raw) {
				id, err := hex.DecodeString(o.Groups[i].ID)
				if err == nil && len(id) == borgrec.GroupIDLen {
					subject.Groups = append(subject.Groups, [borgrec.GroupIDLen]byte(id))
				}
				break
			}
		}
	}
	eff := borgrec.Effective(subject, set, time.Now())
	allowed, err := borgrec.Allows(eff, permission)
	if err != nil {
		return err
	}
	verdict := "DENY "
	if allowed {
		verdict = "ALLOW"
	}
	fmt.Fprintf(g.stdout, "%s   %s %s\n", verdict, key, permission)
	fmt.Fprintf(g.stdout, "holds   %s\n", strings.Join(eff, " "))
	g.say("this is what the organisation's own state says; a service decides from a host's answers and its own headers")
	return nil
}
