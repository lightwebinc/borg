package borgrec

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// Policy record keys (spec section 5A).
const (
	PKMagic       = 0
	PKHall        = 1
	PKOrg         = 2
	PKKind        = 3
	PKRole        = 4
	PKPermissions = 5
	PKTo          = 6
	PKExpiry      = 7
	PKCreated     = 8
	pkLast        = PKCreated
)

// The two kinds of policy record.
const (
	// PolicyRole defines a role and the permissions it carries.
	PolicyRole uint64 = 0
	// PolicyBinding gives a role to a group or to one identity key.
	PolicyBinding uint64 = 1
)

// Policy is one policy record: the payload of a policy carrier's one output.
// It is either a role definition or a binding, by Kind. The organisation's
// version 2 token commits to the set of them by its policy root, so what a
// role may do is a committed fact of the chain and not a claim of whoever
// serves it.
//
// A policy record is in the clear: nothing in it is encrypted, so every host
// and every follower of the hall reads an organisation's roles and bindings
// (spec section 16.16).
type Policy struct {
	Hall        string   // key 1
	Org         []byte   // key 2, the organisation's identity key, and the carrier's owner
	Kind        uint64   // key 3, PolicyRole or PolicyBinding
	Role        string   // key 4, the role this record defines or binds
	Permissions []string // key 5, a definition's permissions, or a binding's attenuation
	To          []byte   // key 6, a binding's group id (16 bytes) or identity key (33); nil is CBOR null
	Expiry      string   // key 7, a binding's expiry; "" is CBOR null
	Created     uint64   // key 8, Unix seconds, a claim
	// Extra holds integer keys above 8, preserved verbatim and ignored.
	Extra cbor.Map
}

// IsBinding reports whether the record binds a role rather than defining one.
func (p *Policy) IsBinding() bool { return p.Kind == PolicyBinding }

// BoundToGroup reports whether the record binds its role to a group, which is
// a To of GroupIDLen bytes, rather than to one identity key.
func (p *Policy) BoundToGroup() bool { return p.IsBinding() && len(p.To) == GroupIDLen }

// Validate applies every rule that needs only the record itself, in the
// refusal order of spec section 3.1.
func (p *Policy) Validate() error {
	if err := record.CheckExtra(p.Extra, pkLast, pkLast+1); err != nil {
		return err
	}
	if err := CheckHall(p.Hall); err != nil {
		return err
	}
	if err := checkOrgKey(p.Org); err != nil {
		return err
	}
	if err := record.InRange(p.Kind, PolicyRole, PolicyBinding, "kind"); err != nil {
		return err
	}
	if err := CheckRoleName(p.Role); err != nil {
		return err
	}
	if err := checkPermissions(p.Permissions); err != nil {
		return err
	}
	if err := p.checkTo(); err != nil {
		return err
	}
	if p.Expiry != "" {
		if _, err := ParseExpiry(p.Expiry); err != nil {
			return err
		}
	}
	if err := record.InRange(p.Created, 1, MaxTime, "created"); err != nil {
		return err
	}
	return p.cross()
}

// checkPermissions is key 5's rule: at most MaxPermissions members, strictly
// ascending by bytes so one set has one encoding, each one a permission.
func checkPermissions(a []string) error {
	if len(a) > MaxPermissions {
		return fmt.Errorf("%w: permissions", ErrList)
	}
	for i, s := range a {
		if i > 0 && a[i-1] >= s {
			return fmt.Errorf("%w: permissions", ErrList)
		}
		if _, err := ParsePermission(s); err != nil {
			return fmt.Errorf("%w: %q", ErrPermission, s)
		}
	}
	return nil
}

// checkTo is key 6's rule: null, a group identifier of GroupIDLen bytes, or
// an identity key that is neither the organisation nor the anyone key.
func (p *Policy) checkTo() error {
	switch len(p.To) {
	case 0:
		return nil
	case GroupIDLen:
		return nil
	case 33:
		if err := CheckIdentity(p.To); err != nil {
			return err
		}
		if IsAnyone(p.To) || bytes.Equal(p.To, p.Org) {
			return fmt.Errorf("%w: a binding to the organisation or the anyone key", ErrIdentity)
		}
		return nil
	}
	return fmt.Errorf("%w: to is neither a group identifier nor an identity key", ErrIdentity)
}

// cross is refusal step 7: a definition carries no subject and no expiry, and
// a binding names a subject.
func (p *Policy) cross() error {
	if p.Kind == PolicyRole && (len(p.To) != 0 || p.Expiry != "") {
		return fmt.Errorf("%w: a role definition has no subject and no expiry", ErrBinding)
	}
	if p.Kind == PolicyBinding && len(p.To) == 0 {
		return fmt.Errorf("%w: a binding names a group or a key", ErrBinding)
	}
	return nil
}

// Encode writes the policy record in canonical CBOR. A To or an Expiry this
// version leaves out is written as CBOR null, so the key is always present.
func (p *Policy) Encode() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	perms := make([]cbor.Value, len(p.Permissions))
	for i, s := range p.Permissions {
		perms[i] = s
	}
	m := cbor.Map{
		{Key: uint64(PKMagic), Val: MagicPolicy},
		{Key: uint64(PKHall), Val: p.Hall},
		{Key: uint64(PKOrg), Val: p.Org},
		{Key: uint64(PKKind), Val: p.Kind},
		{Key: uint64(PKRole), Val: p.Role},
		{Key: uint64(PKPermissions), Val: perms},
		{Key: uint64(PKTo), Val: nullableBytes(p.To)},
		{Key: uint64(PKExpiry), Val: nullableText(p.Expiry)},
		{Key: uint64(PKCreated), Val: p.Created},
	}
	return record.Encode(m, p.Extra, MaxPolicyRecord)
}

func nullableBytes(b []byte) cbor.Value {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

func nullableText(s string) cbor.Value {
	if s == "" {
		return nil
	}
	return s
}

// DecodePolicy parses a policy record in the refusal order.
func DecodePolicy(b []byte) (*Policy, error) {
	f, err := decodeRecord(b, MaxPolicyRecord, pkLast, MagicPolicy)
	if err != nil {
		return nil, err
	}
	p := &Policy{Extra: f.Extra}
	if p.Hall, err = f.Text(PKHall); err != nil {
		return nil, err
	}
	if err := CheckHall(p.Hall); err != nil {
		return nil, err
	}
	if p.Org, err = f.BytesN(PKOrg, -1); err != nil {
		return nil, err
	}
	if err := checkOrgKey(p.Org); err != nil {
		return nil, err
	}
	if p.Kind, err = f.Uint(PKKind, PolicyRole, PolicyBinding); err != nil {
		return nil, err
	}
	if p.Role, err = f.Text(PKRole); err != nil {
		return nil, err
	}
	if err := CheckRoleName(p.Role); err != nil {
		return nil, err
	}
	if p.Permissions, err = f.textList(PKPermissions, MaxPermissions); err != nil {
		return nil, err
	}
	if err := checkPermissions(p.Permissions); err != nil {
		return nil, err
	}
	if p.To, err = f.nullableBytes(PKTo); err != nil {
		return nil, err
	}
	if err := p.checkTo(); err != nil {
		return nil, err
	}
	if p.Expiry, err = f.nullableText(PKExpiry); err != nil {
		return nil, err
	}
	if p.Expiry != "" {
		if _, err := ParseExpiry(p.Expiry); err != nil {
			return nil, err
		}
	}
	if p.Created, err = f.Uint(PKCreated, 1, MaxTime); err != nil {
		return nil, err
	}
	if err := p.cross(); err != nil {
		return nil, err
	}
	return p, nil
}

// Subject is what a service has established about one subject before it asks
// what that subject may do. Every field is a conclusion the caller reached by
// the checks of spec section 13: that the subject is a current member, which
// roles its current grants name (read in the clear, revealed or disclosed by
// the subject), and which of the organisation's groups hold it at their
// latest token.
type Subject struct {
	// Key is the subject's identity key.
	Key []byte
	// Member is false unless the caller established a current membership.
	Member bool
	// Roles are the roles the subject's current grants name. A value that is
	// not a well-formed role name names no role.
	Roles []string
	// Groups are the groups of this organisation whose latest token's
	// members include Key.
	Groups [][GroupIDLen]byte
}

// Effective is the effective permission set of a subject at time now, over
// the policy set of the organisation's latest token (spec section 5A.4): the
// union of what the subject's own roles carry and what every binding that
// reaches it carries, attenuated. The caller supplies only records it has
// verified and found current; expiry is the one currency rule applied here,
// because it needs the clock.
//
// Nothing subtracts. The result is ascending and holds no duplicate, so two
// implementations of this contract produce the same set.
func Effective(s Subject, set []*Policy, now time.Time) []string {
	if !s.Member {
		return []string{}
	}
	// A role's permissions are the intersection of every definition of it:
	// more than one definition is equivocation, and the intersection is the
	// only reading that cannot grant more than the organisation meant by
	// either (section 5A.1).
	defined := map[string][]string{}
	for _, p := range set {
		if p.Kind != PolicyRole {
			continue
		}
		if have, ok := defined[p.Role]; ok {
			defined[p.Role] = Intersect(have, p.Permissions)
			continue
		}
		defined[p.Role] = append([]string(nil), p.Permissions...)
	}
	held := map[string]bool{}
	add := func(role string, only []string) {
		perms, ok := defined[role]
		if !ok {
			return
		}
		if len(only) > 0 {
			perms = Intersect(perms, only)
		}
		for _, x := range perms {
			held[x] = true
		}
	}
	for _, role := range s.Roles {
		if CheckRoleName(role) == nil {
			add(role, nil)
		}
	}
	for _, p := range set {
		if p.Kind != PolicyBinding || !p.current(now) {
			continue
		}
		if p.BoundToGroup() {
			for _, g := range s.Groups {
				if bytes.Equal(p.To, g[:]) {
					add(p.Role, p.Permissions)
					break
				}
			}
			continue
		}
		if bytes.Equal(p.To, s.Key) {
			add(p.Role, p.Permissions)
		}
	}
	out := make([]string, 0, len(held))
	for x := range held {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// current reports whether a binding's expiry has not passed, allowing the
// reader's clock the skew a grant's expiry allows (spec section 5.2).
func (p *Policy) current(now time.Time) bool {
	if p.Expiry == "" {
		return true
	}
	t, err := ParseExpiry(p.Expiry)
	if err != nil {
		return false
	}
	return now.Before(t.Add(ExpirySkew))
}

// Allows reports whether a subject's effective set grants the asked
// permission, which carries no wildcard (section 5A.3).
func Allows(effective []string, ask string) (bool, error) { return GrantsAny(effective, ask) }
