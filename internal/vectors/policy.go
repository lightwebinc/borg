package vectors

import (
	"fmt"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/lightwebinc/borg/borgrec"
)

// ---- permission-v1.json ----

func genPermission(_ *Scenario, files map[string][]byte) error {
	type pv struct {
		Value    string `json:"value"`
		Parses   bool   `json:"parses"`
		Service  string `json:"service,omitempty"`
		Action   string `json:"action,omitempty"`
		Resource string `json:"resource"`
		Prefix   bool   `json:"prefix"`
		Why      string `json:"why"`
	}
	longest := "sixteencharacter/thirtytwocharactersexactlyhere00/" + strings.Repeat("r", borgrec.MaxResource) + "*"
	cases := []struct{ s, why string }{
		{"bsecret/release/payroll", "one service, one action, one resource"},
		{"bchat/moderate/general", "another service's own words"},
		{"bstore/put/photos/*", "a resource prefix: the separator is the service's, not the grammar's"},
		{"bstore/get/*", "every resource of one service and action"},
		{"a/b/c", "the shortest permission"},
		{"s/a/x:y.z-_/w", "every character a resource allows"},
		{longest, "the longest permission the grammar allows"},
		{"", "empty"},
		{"a/b", "no resource"},
		{"/a/b", "no service"},
		{"a//b", "no action"},
		{"a/b/", "an empty resource"},
		{"A/b/c", "upper case in the service"},
		{"a/b/C", "upper case in the resource"},
		{"a_b/c/d", "an underscore in the service"},
		{"*/a/b", "a wildcard service: no permission is everything everywhere"},
		{"a/*/b", "a wildcard action"},
		{"a/b/c*d", "a wildcard inside a resource"},
		{"a/b/**", "two wildcards"},
		{"a/b/c d", "a space"},
		{strings.Repeat("s", borgrec.MaxService+1) + "/a/b", "a service of 17 characters"},
		{"s/" + strings.Repeat("a", borgrec.MaxAction+1) + "/b", "an action of 33 characters"},
		{"s/a/" + strings.Repeat("r", borgrec.MaxResource+1), "a resource of 193 characters"},
	}
	var out []pv
	for _, c := range cases {
		p, err := borgrec.ParsePermission(c.s)
		v := pv{Value: c.s, Parses: err == nil, Why: c.why}
		if err == nil {
			v.Service, v.Action, v.Resource, v.Prefix = p.Service, p.Action, p.Resource, p.Prefix
			if p.String() != c.s {
				return fmt.Errorf("permission %q round trips as %q", c.s, p.String())
			}
		}
		out = append(out, v)
	}
	type mv struct {
		Held   string `json:"held"`
		Asked  string `json:"asked"`
		Grants bool   `json:"grants"`
		Why    string `json:"why"`
	}
	matches := []struct {
		held, asked, why string
	}{
		{"s/a/b", "s/a/b", "exact"},
		{"s/a/b", "s/a/bc", "an exact permission is not a prefix"},
		{"s/a/b", "s/x/b", "the action always matches exactly"},
		{"s/a/b", "t/a/b", "the service always matches exactly"},
		{"s/a/b*", "s/a/b", "a prefix matches the resource it is a prefix of"},
		{"s/a/b*", "s/a/b/c/d", "a prefix matches below the separator"},
		{"s/a/b*", "s/a/ab", "a prefix is not a suffix and not a substring"},
		{"s/a/*", "s/a/anything/at/all", "every resource of one service and action"},
		{"s/a/*", "s/x/anything", "but only that action"},
		{"bstore/put/photos/*", "bstore/put/photos/2026/a.jpg", "the resource a service names"},
		{"bstore/put/photos/*", "bstore/put/photo", "a shorter resource is not under the prefix"},
	}
	var mout []mv
	for _, c := range matches {
		h, err := borgrec.ParsePermission(c.held)
		if err != nil {
			return fmt.Errorf("held %q: %w", c.held, err)
		}
		a, err := borgrec.Asked(c.asked)
		if err != nil {
			return fmt.Errorf("asked %q: %w", c.asked, err)
		}
		mout = append(mout, mv{c.held, c.asked, h.Grants(a), c.why})
	}
	type av struct {
		Value string `json:"value"`
		Ok    bool   `json:"ok"`
		Why   string `json:"why"`
	}
	var asked []av
	for _, c := range []struct{ s, why string }{
		{"s/a/b", "a service asks for one thing"},
		{"s/a/*", "a service may not ask with a wildcard: that is a programming error, not a question"},
		{"s/a/b*", "nor with a prefix"},
	} {
		_, err := borgrec.Asked(c.s)
		asked = append(asked, av{c.s, err == nil, c.why})
	}
	type rv struct {
		Value string `json:"value"`
		Ok    bool   `json:"ok"`
		Why   string `json:"why"`
	}
	var roles []rv
	for _, c := range []struct{ s, why string }{
		{"releaser", "a role name"},
		{"on-call", "a hyphen"},
		{"team_1.a", "an underscore, a digit and a dot"},
		{strings.Repeat("r", borgrec.MaxRoleName), "the longest role name"},
		{"", "empty"},
		{"Releaser", "upper case: a role name carries no case, so two roles cannot be shown alike and read apart"},
		{"on call", "a space"},
		{"röle", "not ASCII"},
		{"admin‍", "a zero-width joiner"},
		{strings.Repeat("r", borgrec.MaxRoleName+1), "65 characters"},
	} {
		roles = append(roles, rv{c.s, borgrec.CheckRoleName(c.s) == nil, c.why})
	}
	type iv struct {
		Role, Keep, Kept []string
		Why              string
	}
	role := []string{"a/a/a", "b/b/b", "c/c/c"}
	var inter []map[string]any
	for _, c := range []struct {
		keep []string
		why  string
	}{
		{nil, "an empty attenuation keeps the whole role"},
		{[]string{"b/b/b"}, "one of the role's permissions"},
		{[]string{"d/d/d"}, "a permission the role does not carry is not added"},
		{[]string{"a/a/a*"}, "a prefix is not the permission it is a prefix of: attenuation is by value"},
	} {
		kept := borgrec.Intersect(role, c.keep)
		if kept == nil {
			kept = []string{}
		}
		inter = append(inter, map[string]any{"role": role, "keep": c.keep, "kept": kept, "why": c.why})
	}
	return put(files, "permission-v1.json", map[string]any{
		"grammar":     "service/action/resource, ASCII and lower case, at most 256 bytes; the resource may end in * and nothing else may",
		"permissions": out,
		"matches":     mout,
		"asked":       asked,
		"roleNames":   roles,
		"intersect":   inter,
	})
}

// ---- policy-v1.json ----

// policyScenario is the policy set the vectors use: two role definitions,
// four bindings, and a second definition of one role, which a reader takes
// as the intersection of the two.
type policyCase struct {
	name string
	rec  *borgrec.Policy
	why  string
}

func policyCases(gid [16]byte) []policyCase {
	p := func(kind uint64, role string, perms []string, to []byte, expiry string) *borgrec.Policy {
		return &borgrec.Policy{Hall: Hall, Org: Org.Pub(), Kind: kind, Role: role,
			Permissions: perms, To: to, Expiry: expiry, Created: Created}
	}
	return []policyCase{
		{"def-releaser", p(borgrec.PolicyRole, "releaser",
			[]string{"bsecret/release/payroll", "bstore/get/*"}, nil, ""),
			"a role definition: the permissions the role carries"},
		{"def-moderator", p(borgrec.PolicyRole, "moderator",
			[]string{"bchat/moderate/general"}, nil, ""),
			"a second role"},
		{"def-releaser-again", p(borgrec.PolicyRole, "releaser",
			[]string{"bstore/get/*", "bstore/put/*"}, nil, ""),
			"a second definition of one role: the organisation has equivocated, and a reader takes the intersection"},
		{"bind-releaser-group", p(borgrec.PolicyBinding, "releaser", nil, gid[:], ""),
			"a binding to a group: every current member of it holds the role"},
		{"bind-moderator-alice", p(borgrec.PolicyBinding, "moderator", nil, Alice.Pub(), "2030-01-02T03:04:05Z"),
			"a binding to one key, with an expiry"},
		{"bind-releaser-bob", p(borgrec.PolicyBinding, "releaser", []string{"bstore/get/*"}, Bob.Pub(), ""),
			"an attenuated binding: it keeps one of the role's permissions and can never widen it"},
		{"bind-archivist-carol", p(borgrec.PolicyBinding, "archivist", nil, Carol.Pub(), ""),
			"a binding whose role no definition defines: admitted, and it authorises nothing"},
		{"bind-moderator-carol", p(borgrec.PolicyBinding, "moderator", nil, Carol.Pub(), "2020-01-01T00:00:00Z"),
			"a binding whose expiry has passed"},
	}
}

func genPolicy(s *Scenario, files map[string][]byte) error {
	gid := GroupID()
	cases := policyCases(gid)

	type recVec struct {
		Name        string   `json:"name"`
		Kind        uint64   `json:"kind"`
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
		To          string   `json:"to,omitempty"`
		ToKind      string   `json:"toKind,omitempty"`
		Expiry      string   `json:"expiry,omitempty"`
		Commitment  string   `json:"commitment"`
		Hex         string   `json:"hex"`
		Why         string   `json:"why"`
	}
	var recs []recVec
	commit := map[string][32]byte{}
	for _, c := range cases {
		b, err := c.rec.Encode()
		if err != nil {
			return fmt.Errorf("policy %s: %w", c.name, err)
		}
		if _, err := borgrec.DecodePolicy(b); err != nil {
			return fmt.Errorf("policy %s does not decode: %w", c.name, err)
		}
		cm := label("commitment/policy/%s", c.name)
		commit[c.name] = cm
		v := recVec{Name: c.name, Kind: c.rec.Kind, Role: c.rec.Role, Permissions: c.rec.Permissions,
			Expiry: c.rec.Expiry, Commitment: hx32(cm), Hex: hx(b), Why: c.why}
		if v.Permissions == nil {
			v.Permissions = []string{}
		}
		if c.rec.To != nil {
			v.To = hx(c.rec.To)
			v.ToKind = "key"
			if c.rec.BoundToGroup() {
				v.ToKind = "group"
			}
		}
		recs = append(recs, v)
		putHex(files, "policy-"+c.name+".hex", b)
	}

	// A chain of two organisation tokens that carries the set: the first
	// defines five records, the second defines two more and retires one.
	first := []string{"def-releaser", "def-moderator", "bind-releaser-group", "bind-moderator-alice", "bind-releaser-bob"}
	later := []string{"def-releaser-again", "bind-archivist-carol"}
	retired := []string{"bind-moderator-alice"}
	define0 := make([][32]byte, 0, len(first))
	for _, n := range first {
		define0 = append(define0, commit[n])
	}
	define0 = sorted32(define0...)
	set0 := append([][32]byte(nil), define0...)

	define1 := [][32]byte{commit[later[0]], commit[later[1]]}
	define1 = sorted32(define1...)
	retire1 := [][32]byte{commit[retired[0]]}
	set1 := make([][32]byte, 0, len(set0))
	for _, c := range set0 {
		if c != retire1[0] {
			set1 = append(set1, c)
		}
	}
	set1 = sorted32(append(set1, define1...)...)

	o0 := &borgrec.Org{Hall: Hall, Org: Org.Pub(), Seq: 0, Name: "Example Organisation",
		Admit: s.Orgs[0].Admit, Count: s.Orgs[0].Count, Roll: s.Orgs[0].Roll, Created: Created,
		Define: define0, PolicyCount: uint64(len(set0)), Policy: borgrec.PolicyRoot(set0)}
	o1 := &borgrec.Org{Hall: Hall, Org: Org.Pub(), Seq: 1, Prev: label("txid/policy-org/0"),
		Name: "Example Organisation", Count: s.Orgs[0].Count, Roll: s.Orgs[0].Roll, Created: Created + 7200,
		Define: define1, Retire: retire1, PolicyCount: uint64(len(set1)), Policy: borgrec.PolicyRoot(set1)}
	rec0, err := o0.Encode()
	if err != nil {
		return fmt.Errorf("policy org 0: %w", err)
	}
	rec1, err := o1.Encode()
	if err != nil {
		return fmt.Errorf("policy org 1: %w", err)
	}
	if err := borgrec.CheckOrgTransition(o0, label("txid/policy-org/0"), o1); err != nil {
		return fmt.Errorf("policy org transition: %w", err)
	}
	fold := borgrec.NewOrgFold()
	for i, o := range []*borgrec.Org{o0, o1} {
		if st := fold.Apply(o); st != borgrec.StatusConsistent {
			return fmt.Errorf("policy org %d folds as %s", i, st)
		}
	}

	// An inclusion context into the policy root, the shape a role or bound
	// answer carries: one that verifies, and one held against the wrong root.
	target := commit["bind-releaser-group"]
	ctx, err := borgrec.ProvePolicy(1, set1, target)
	if err != nil {
		return err
	}
	ctxEnc, err := ctx.Encode()
	if err != nil {
		return err
	}
	if !ctx.VerifyPolicy(target, borgrec.PolicyRoot(set1)) {
		return fmt.Errorf("the policy context does not verify against its own root")
	}
	if ctx.VerifyPolicy(target, borgrec.PolicyRoot(set0)) {
		return fmt.Errorf("the policy context verified against the earlier root")
	}

	// Commitments are in hash byte order here, as org-v1.json writes them:
	// a reader hashes them into a root rather than printing them.
	setHex := func(a [][32]byte) []string {
		out := make([]string, len(a))
		for i, c := range a {
			out[i] = hx32(c)
		}
		return out
	}
	return put(files, "policy-v1.json", map[string]any{
		"records": recs,
		"chain": []map[string]any{
			{"seq": 0, "hex": hx(rec0), "define": setHex(define0), "retire": []string{},
				"set": setHex(set0), "policy": hx32(borgrec.PolicyRoot(set0)),
				"why": "the first token defines its whole policy set and retires nothing"},
			{"seq": 1, "hex": hx(rec1), "define": setHex(define1), "retire": setHex(retire1),
				"set": setHex(set1), "policy": hx32(borgrec.PolicyRoot(set1)),
				"why": "a transition that defines two records and retires one"},
		},
		"context": map[string]any{
			"of": hx32(target), "seq": 1, "hex": hx(ctxEnc),
			"root": hx32(borgrec.PolicyRoot(set1)), "wrongRoot": hx32(borgrec.PolicyRoot(set0)),
			"why": "the context a role or bound answer carries, which verifies against the policy root of its seq and against no other",
		},
		"intersection": map[string]any{
			"role": "releaser",
			"of":   []string{"def-releaser", "def-releaser-again"},
			"kept": borgrec.Intersect(cases[0].rec.Permissions, cases[2].rec.Permissions),
			"why":  "two definitions of one role are read as the intersection of their permissions, the only reading that cannot grant more than the organisation meant by either",
		},
	})
}

func hx32(b [32]byte) string { return hx(b[:]) }

// policyRecords encodes the policy cases by name, for the refusal vectors.
func policyRecords() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, c := range policyCases(GroupID()) {
		b, err := c.rec.Encode()
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", c.name, err)
		}
		out[c.name] = b
	}
	return out, nil
}

// manyPermissions is n distinct permissions in ascending order, for the
// bound cases.
func manyPermissions(n int) []cbor.Value {
	out := make([]cbor.Value, n)
	for i := range out {
		out[i] = fmt.Sprintf("s/a/%c%c", 'a'+i/26, 'a'+i%26)
	}
	return out
}

// withNull sets key k to CBOR null, which mutate cannot do: it reads a nil
// value as a key to remove, and null is a value this version writes.
func withNull(rec []byte, k uint64) []byte {
	m := asMap(rec)
	out := make(cbor.Map, 0, len(m))
	for _, p := range m {
		if p.Key == k {
			out = append(out, cbor.Pair{Key: k, Val: nil})
			continue
		}
		out = append(out, p)
	}
	b, err := cbor.Encode(out)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- authz-v1.json ----

// genAuthz pins the effective-permission procedure of spec section 5A.4
// over the policy set of policy-v1.json: who holds what, and what three
// asked permissions come to for each subject.
func genAuthz(_ *Scenario, files map[string][]byte) error {
	gid := GroupID()
	cases := policyCases(gid)
	set := make([]*borgrec.Policy, 0, len(cases))
	byName := map[string]*borgrec.Policy{}
	for _, c := range cases {
		byName[c.name] = c.rec
		// bind-moderator-alice is retired by the chain's second token, so
		// the set a reader folds at the latest token leaves it out.
		if c.name == "bind-moderator-alice" {
			continue
		}
		set = append(set, c.rec)
	}
	now, err := borgrec.ParseExpiry("2026-06-01T00:00:00Z")
	if err != nil {
		return err
	}
	type subject struct {
		Name      string   `json:"name"`
		Key       string   `json:"key"`
		Member    bool     `json:"member"`
		Roles     []string `json:"roles"`
		Groups    []string `json:"groups"`
		Effective []string `json:"effective"`
		Asked     []struct {
			Permission string `json:"permission"`
			Allowed    bool   `json:"allowed"`
		} `json:"asked"`
		Why string `json:"why"`
	}
	ask := []string{"bsecret/release/payroll", "bstore/get/photos/a.jpg", "bchat/moderate/general"}
	subjects := []struct {
		name string
		s    borgrec.Subject
		why  string
	}{
		{"alice", borgrec.Subject{Key: Alice.Pub(), Member: true, Roles: []string{"releaser"}},
			"her grant names releaser, which two definitions define: a reader takes their intersection, so the role carries less than either says alone"},
		{"alice-in-group", borgrec.Subject{Key: Alice.Pub(), Member: true, Groups: [][16]byte{gid}},
			"the same key with no role of its own: everything it holds comes from the binding to its group"},
		{"bob", borgrec.Subject{Key: Bob.Pub(), Member: true, Roles: []string{"member"}},
			"his grant names no role any definition defines; his own binding is attenuated, and attenuation keeps the intersection"},
		{"carol", borgrec.Subject{Key: Carol.Pub(), Member: true, Roles: []string{"member"}},
			"the bindings that name her are one whose role nothing defines and one whose expiry has passed, so she holds nothing"},
		{"alice-shouting", borgrec.Subject{Key: Alice.Pub(), Member: true, Roles: []string{"Releaser"}},
			"a role field that is not a well-formed role name names no role, whatever it looks like"},
		{"dave", borgrec.Subject{Key: Dave.Pub(), Member: false, Roles: []string{"releaser"}, Groups: [][16]byte{gid}},
			"never a member: no grant, no permissions, whatever any binding says"},
	}
	var out []subject
	for _, c := range subjects {
		eff := borgrec.Effective(c.s, set, now)
		v := subject{Name: c.name, Key: hx(c.s.Key), Member: c.s.Member, Roles: c.s.Roles,
			Groups: []string{}, Effective: eff, Why: c.why}
		if v.Roles == nil {
			v.Roles = []string{}
		}
		for _, g := range c.s.Groups {
			v.Groups = append(v.Groups, hx(g[:]))
		}
		for _, a := range ask {
			allowed, err := borgrec.Allows(eff, a)
			if err != nil {
				return fmt.Errorf("%s asks %q: %w", c.name, a, err)
			}
			v.Asked = append(v.Asked, struct {
				Permission string `json:"permission"`
				Allowed    bool   `json:"allowed"`
			}{a, allowed})
		}
		out = append(out, v)
	}
	if _, err := borgrec.Allows([]string{"a/a/a"}, "a/a/*"); err == nil {
		return fmt.Errorf("a wildcard was asked and answered")
	}
	return put(files, "authz-v1.json", map[string]any{
		"now":      "2026-06-01T00:00:00Z",
		"set":      []string{"def-releaser", "def-moderator", "def-releaser-again", "bind-releaser-group", "bind-releaser-bob", "bind-archivist-carol", "bind-moderator-carol"},
		"subjects": out,
		"rules":    "the union of the roles a subject's grants name and every current binding that reaches it, attenuated; nothing subtracts, and a subject that is not a current member holds nothing",
	})
}
