package borgrec

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Scope tokens this version defines (spec section 5.3). Every other verb is
// unknown: it authorises nothing, and a known verb with a qualifier this
// version does not define is denied, never read as the bare verb.
const (
	VerbMember = "member"
	VerbAdmin  = "admin"
)

// Token is one scope token: a verb and an optional qualifier.
type Token struct {
	Verb      string
	Qualifier string
	HasQual   bool
}

func isLowerDigit(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

// ParseScope applies BRC-169 section 9.2's grammar with one restriction: the
// whole-scope "*" is refused, because a membership grant never confers
// unrestricted authority.
//
//	scope       = scope-token *( "," scope-token )
//	scope-token = verb [ ":" qualifier ]
//	verb        = 1*32( lower / DIGIT / "-" )
//	qualifier   = 1*64( lower / DIGIT / "-" / "_" / "." )
func ParseScope(s string) ([]Token, error) {
	if len(s) == 0 || len(s) > MaxScope {
		return nil, ErrScope
	}
	parts := strings.Split(s, ",")
	out := make([]Token, 0, len(parts))
	for _, p := range parts {
		verb, qual, has := strings.Cut(p, ":")
		if len(verb) < 1 || len(verb) > 32 {
			return nil, ErrScope
		}
		for i := 0; i < len(verb); i++ {
			if !isLowerDigit(verb[i]) && verb[i] != '-' {
				return nil, ErrScope
			}
		}
		if has {
			if len(qual) < 1 || len(qual) > 64 {
				return nil, ErrScope
			}
			for i := 0; i < len(qual); i++ {
				c := qual[i]
				if !isLowerDigit(c) && c != '-' && c != '_' && c != '.' {
					return nil, ErrScope
				}
			}
		}
		out = append(out, Token{Verb: verb, Qualifier: qual, HasQual: has})
	}
	return out, nil
}

// Grants reports whether a parsed scope authorises verb with no qualifier.
// It fails closed (BRC-169 section 9.2 rule 4): a token for verb that
// carries a qualifier this version does not define denies verb outright,
// rather than being read as the bare verb.
func Grants(tokens []Token, verb string) bool {
	found := false
	for _, t := range tokens {
		if t.Verb != verb {
			continue
		}
		if t.HasQual {
			return false
		}
		found = true
	}
	return found
}

// IsMembership reports whether a scope makes its grant a membership: it
// parses and grants "member".
func IsMembership(scope string) bool {
	t, err := ParseScope(scope)
	return err == nil && Grants(t, VerbMember)
}

// ExpiryLayout is the one spelling of a grant's expiry: ISO-8601 UTC to the
// second (BRC-169 section 9.1's expiry, its format pinned).
const ExpiryLayout = "2006-01-02T15:04:05Z"

// ParseExpiry applies the expiry rule: exactly YYYY-MM-DDTHH:MM:SSZ, a real
// UTC date and time, from 1970 to 9999.
func ParseExpiry(s string) (time.Time, error) {
	if len(s) != len(ExpiryLayout) {
		return time.Time{}, ErrExpiry
	}
	t, err := time.Parse(ExpiryLayout, s)
	if err != nil || t.Format(ExpiryLayout) != s || t.Unix() < 0 {
		return time.Time{}, ErrExpiry
	}
	return t, nil
}

// CheckRole applies the role rule: 1 to MaxRole bytes of UTF-8 text. A role
// is a label a client shows; it authorises nothing.
func CheckRole(s string) error {
	if len(s) < 1 || len(s) > MaxRole || !utf8.ValidString(s) {
		return ErrRole
	}
	return nil
}

// CheckFieldValue applies the rule of a grant field to its decrypted value.
func CheckFieldValue(name, value string) error {
	switch name {
	case "expiry":
		_, err := ParseExpiry(value)
		return err
	case "role":
		return CheckRole(value)
	case "scope":
		_, err := ParseScope(value)
		return err
	}
	return fmt.Errorf("%w: %q", ErrFields, name)
}
