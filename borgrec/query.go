package borgrec

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Class is one lookup question class of ls_borg (spec section 10.2).
type Class struct {
	Name    string
	Members []string // sorted
	Free    bool
	Limit   int // most outputs one answer holds
}

// Classes is every class this version defines. A question carries exactly
// the members of one of them.
var Classes = []Class{
	{"org", []string{"hall", "org"}, true, 8},
	{"org-seq", []string{"hall", "org", "seq"}, true, 8},
	{"roll", []string{"hall", "roll"}, true, 64},
	{"roll-after", []string{"after", "hall", "roll"}, true, 64},
	{"member", []string{"hall", "member", "org"}, true, 8},
	{"policy", []string{"hall", "policy"}, true, 64},
	{"policy-after", []string{"after", "hall", "policy"}, true, 64},
	{"role", []string{"hall", "org", "role"}, true, 64},
	{"role-after", []string{"after", "hall", "org", "role"}, true, 64},
	{"bound", []string{"boundTo", "hall", "org"}, true, 64},
	{"bound-after", []string{"after", "boundTo", "hall", "org"}, true, 64},
	{"groups", []string{"groupsOf", "hall"}, true, 64},
	{"groups-after", []string{"after", "groupsOf", "hall"}, true, 64},
	{"member-of", []string{"hall", "memberOf", "org"}, true, 64},
	{"member-of-after", []string{"after", "hall", "memberOf", "org"}, true, 64},
	{"group", []string{"group", "hall", "org"}, true, 8},
	{"group-seq", []string{"group", "hall", "org", "seq"}, true, 8},
	{"wraps", []string{"epoch", "group", "hall", "org"}, true, 64},
	{"wraps-after", []string{"after", "epoch", "group", "hall", "org"}, true, 64},
	{"wrap", []string{"epoch", "group", "hall", "member", "org"}, true, 8},
	{"sweep", []string{"hall", "spent"}, true, 8},
	{"history", []string{"chain", "hall", "history"}, false, 64},
	{"history-after", []string{"after", "chain", "hall", "history"}, false, 64},
}

// Question is a validated question: its class and its members.
type Question struct {
	Class   Class
	Members map[string]string
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func checkKeyHex(s string) error {
	if !isLowerHex(s, 66) {
		return ErrQuery
	}
	b, _ := hex.DecodeString(s)
	if CheckIdentity(b) != nil {
		return ErrQuery
	}
	return nil
}

// checkDecimal accepts a decimal integer from lo to hi with no sign and no
// leading zero.
func checkDecimal(s string, lo, hi uint64) error {
	if s == "" || len(s) > 16 || (len(s) > 1 && s[0] == '0') {
		return ErrQuery
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return ErrQuery
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n < lo || n > hi {
		return ErrQuery
	}
	return nil
}

// checkOutpoint accepts BRC-100's OutpointString: a display-order txid, a
// dot, and the output index in decimal with no leading zero.
func checkOutpoint(s string) error {
	txid, vout, ok := strings.Cut(s, ".")
	if !ok || !isLowerHex(txid, 64) {
		return ErrQuery
	}
	return checkDecimal(vout, 0, MaxEpoch)
}

// checkMember applies one member's value rule.
func checkMember(class, name, v string) error {
	switch name {
	case "hall":
		if CheckHall(v) != nil {
			return ErrQuery
		}
		return nil
	case "org", "roll", "member", "groupsOf", "memberOf", "history", "policy":
		return checkKeyHex(v)
	case "role":
		if CheckRoleName(v) != nil {
			return ErrQuery
		}
		return nil
	case "boundTo":
		// A binding names a group identifier or an identity key, and the
		// two are told apart by their length, as the record's `to` is.
		if isLowerHex(v, 2*GroupIDLen) {
			return nil
		}
		return checkKeyHex(v)
	case "group":
		if !isLowerHex(v, 2*GroupIDLen) {
			return ErrQuery
		}
		return nil
	case "chain":
		if v == "org" || isLowerHex(v, 2*GroupIDLen) {
			return nil
		}
		return ErrQuery
	case "seq":
		if v == "0" {
			return nil
		}
		return checkDecimal(v, 1, MaxSafe)
	case "epoch":
		if v == "0" {
			return nil
		}
		return checkDecimal(v, 1, MaxEpoch)
	case "spent":
		return checkOutpoint(v)
	case "after":
		switch class {
		case "roll-after", "wraps-after", "policy-after", "role-after", "bound-after":
			if isLowerHex(v, 64) {
				return nil
			}
		case "groups-after", "member-of-after":
			if isLowerHex(v, 2*GroupIDLen) {
				return nil
			}
		case "history-after":
			if v == "0" {
				return nil
			}
			return checkDecimal(v, 1, MaxSafe)
		}
		return ErrQuery
	}
	return ErrQuery
}

// ParseQuery validates a BRC-24 query object as parsed: a JSON object whose
// members are all strings (a member given twice takes its last value, as
// JSON parsers do), whose member set is exactly one class's, and whose
// values meet their rules. Anything else is refused, never answered empty.
func ParseQuery(raw []byte) (*Question, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("%w: not a JSON object", ErrQuery)
	}
	vals := make(map[string]string, len(m))
	names := make([]string, 0, len(m))
	for k, v := range m {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("%w: member %q is not a string", ErrQuery, k)
		}
		vals[k] = s
		names = append(names, k)
	}
	sort.Strings(names)
	for _, c := range Classes {
		if strings.Join(c.Members, ",") != strings.Join(names, ",") {
			continue
		}
		for _, n := range names {
			if err := checkMember(c.Name, n, vals[n]); err != nil {
				return nil, fmt.Errorf("%w: member %q", ErrQuery, n)
			}
		}
		return &Question{Class: c, Members: vals}, nil
	}
	return nil, fmt.Errorf("%w: no class has these members", ErrQuery)
}
