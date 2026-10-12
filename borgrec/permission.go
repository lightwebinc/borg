package borgrec

import "strings"

// Permission bounds (spec section 5A.2). The grammar's longest permission is
// MaxService + 1 + MaxAction + 1 + MaxResource + 1 = 243 bytes; MaxPermission
// is the bound a reader holds the text to before it parses.
const (
	MaxPermission = 256
	MaxService    = 16
	MaxAction     = 32
	MaxResource   = 192
	// MaxPermissions bounds the permissions one policy record carries.
	MaxPermissions = 24
	// MaxRoleName bounds a role name, the same bound as a grant's role field.
	MaxRoleName = MaxRole
)

// Permission is one parsed permission: what a service, an action and a
// resource a subject may be granted. Prefix is set when the resource ended
// in "*", which makes it a prefix rather than a whole resource.
//
//	permission = service "/" action "/" resource
//	service    = 1*16( lower / DIGIT / "-" )
//	action     = 1*32( lower / DIGIT / "-" )
//	resource   = "*" / ( 1*192( lower / DIGIT / "-" / "_" / "." / "/" / ":" ) [ "*" ] )
//
// The grammar is ASCII and lower case, so no two permissions can look alike
// and every implementation compares them as bytes. A service and an action
// are never "*", so no permission is everything everywhere.
type Permission struct {
	Service  string
	Action   string
	Resource string
	Prefix   bool
}

func isResourceByte(c byte) bool {
	return isLowerDigit(c) || c == '-' || c == '_' || c == '.' || c == '/' || c == ':'
}

func checkRun(s string, max int, ok func(byte) bool) bool {
	if len(s) < 1 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

// ParsePermission applies the grammar of spec section 5A.2. A service and an
// action hold no "/", so the first two separators split the three parts even
// where the resource holds further ones.
func ParsePermission(s string) (Permission, error) {
	if len(s) > MaxPermission {
		return Permission{}, ErrPermission
	}
	service, rest, found := strings.Cut(s, "/")
	if !found {
		return Permission{}, ErrPermission
	}
	action, resource, found := strings.Cut(rest, "/")
	if !found {
		return Permission{}, ErrPermission
	}
	p := Permission{Service: service, Action: action}
	if !checkRun(service, MaxService, func(c byte) bool { return isLowerDigit(c) || c == '-' }) ||
		!checkRun(action, MaxAction, func(c byte) bool { return isLowerDigit(c) || c == '-' }) {
		return Permission{}, ErrPermission
	}
	if resource == "*" {
		p.Prefix = true
		return p, nil
	}
	if strings.HasSuffix(resource, "*") {
		p.Prefix = true
		resource = resource[:len(resource)-1]
	}
	if !checkRun(resource, MaxResource, isResourceByte) {
		return Permission{}, ErrPermission
	}
	p.Resource = resource
	return p, nil
}

// String writes the permission back, which is the one spelling a reader
// accepted: ParsePermission(p.String()) == p for every parsed permission.
func (p Permission) String() string {
	r := p.Resource
	if p.Prefix {
		r += "*"
	}
	return p.Service + "/" + p.Action + "/" + r
}

// Grants reports whether the held permission p grants the asked one. The
// service and the action always match exactly; a held resource that ended in
// "*" matches by prefix, and nothing else matches: there is no suffix
// wildcard, none inside a resource, and no hierarchy a separator implies.
//
// The asked permission must carry no "*" (Asked below); Grants reads one that
// does as a resource of its own and so grants nothing by accident.
func (p Permission) Grants(asked Permission) bool {
	if p.Service != asked.Service || p.Action != asked.Action || asked.Prefix {
		return false
	}
	if p.Prefix {
		return strings.HasPrefix(asked.Resource, p.Resource)
	}
	return p.Resource == asked.Resource
}

// Asked parses a permission a service asks about. It is stricter than
// ParsePermission: a "*" anywhere in it is a programming error, so it is
// refused rather than treated as a character or as a wildcard.
func Asked(s string) (Permission, error) {
	p, err := ParsePermission(s)
	if err != nil {
		return Permission{}, err
	}
	if p.Prefix {
		return Permission{}, ErrPermission
	}
	return p, nil
}

// GrantsAny reports whether any held permission grants the asked one. held is
// the text of an effective set; a member of it that breaks the grammar is
// refused, because a reader that met one has read a record no host admits.
func GrantsAny(held []string, asked string) (bool, error) {
	a, err := Asked(asked)
	if err != nil {
		return false, err
	}
	for _, h := range held {
		p, err := ParsePermission(h)
		if err != nil {
			return false, err
		}
		if p.Grants(a) {
			return true, nil
		}
	}
	return false, nil
}

// CheckRoleName applies the role-name grammar of spec section 5A.1: 1 to 64
// of lower-case ASCII letters, digits, "-", "_" and ".", the characters
// BRC-169 section 9.2 allows a scope qualifier. A role name is an identifier
// compared byte for byte, so it carries no case and no character a renderer
// would strip; an organization's human wording is in its display names.
func CheckRoleName(s string) error {
	if !checkRun(s, MaxRoleName, func(c byte) bool {
		return isLowerDigit(c) || c == '-' || c == '_' || c == '.'
	}) {
		return ErrRoleName
	}
	return nil
}

// Intersect keeps the permissions of a that b also carries, in a's order.
// Attenuating a binding is this over the role's set, so a binding can never
// widen a role, and two definitions of one role are read as this over both.
func Intersect(a, b []string) []string {
	out := make([]string, 0, len(a))
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
				break
			}
		}
	}
	return out
}
