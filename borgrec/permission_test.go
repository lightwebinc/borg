package borgrec

import (
	"strings"
	"testing"
)

const longest = "sixteencharacter/thirtytwocharactersexactlyhere00/" +
	"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr" +
	"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr" +
	"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr*"

// TestPermissionGrammar walks the grammar of spec section 5A.2, each refused
// case breaking exactly one rule.
func TestPermissionGrammar(t *testing.T) {
	if len(longest) != 243 {
		t.Fatalf("the longest permission is %d bytes, the spec says 243", len(longest))
	}
	for _, s := range []string{
		"bsecret/release/payroll",
		"bchat/moderate/general",
		"bstore/put/photos/*",
		"bstore/get/*",
		"a/b/c",
		"s/a/x:y.z-_/w",
		"s9/a-9/r-9_9.9:9/9",
		longest,
	} {
		p, err := ParsePermission(s)
		if err != nil {
			t.Fatalf("%q refused: %v", s, err)
		}
		if p.String() != s {
			t.Fatalf("%q round trips as %q", s, p.String())
		}
	}
	for _, s := range []string{
		"", "a", "a/b", "abc",
		"/a/b", "a//b", "a/b/",
		"A/b/c", "a/B/c", "a/b/C",
		"a_b/c/d", "a/b_c/d",
		"*/a/b", "a/*/b",
		"a/b/c*d", "a/b/**", "a/b/*x",
		"a b/c/d", "a/b c/d", "a/b/c d",
		"a/b/c\n",
		strings.Repeat("s", MaxService+1) + "/a/b",
		"s/" + strings.Repeat("a", MaxAction+1) + "/b",
		"s/a/" + strings.Repeat("r", MaxResource+1),
		"s/a/" + strings.Repeat("r", MaxResource) + "x*",
		"s/a/" + strings.Repeat("r", MaxPermission),
	} {
		if p, err := ParsePermission(s); err == nil {
			t.Fatalf("%q accepted as %+v", s, p)
		} else if Reason(err) != "permission" {
			t.Fatalf("%q refused as %q", s, Reason(err))
		}
	}
}

// TestPermissionMatching pins section 5A.3: the service and the action always
// match exactly, a held resource ending in "*" matches by prefix, and nothing
// else matches.
func TestPermissionMatching(t *testing.T) {
	for _, c := range []struct {
		held, asked string
		want        bool
	}{
		{"s/a/b", "s/a/b", true},
		{"s/a/b", "s/a/bc", false},
		{"s/a/b", "s/a/a", false},
		{"s/a/b", "s/x/b", false},
		{"s/a/b", "t/a/b", false},
		{"s/a/b*", "s/a/b", true},
		{"s/a/b*", "s/a/bc", true},
		{"s/a/b*", "s/a/b/c/d", true},
		{"s/a/b*", "s/a/a", false},
		{"s/a/b*", "s/a/ab", false},
		{"s/a/b*", "t/a/b", false},
		{"s/a/*", "s/a/anything/at/all", true},
		{"s/a/*", "s/x/anything", false},
		{"bstore/put/photos/*", "bstore/put/photos/2026/a.jpg", true},
		{"bstore/put/photos/*", "bstore/put/photo", false},
		{"bstore/put/photos/*", "bstore/get/photos/a", false},
	} {
		h, err := ParsePermission(c.held)
		if err != nil {
			t.Fatalf("held %q: %v", c.held, err)
		}
		a, err := Asked(c.asked)
		if err != nil {
			t.Fatalf("asked %q: %v", c.asked, err)
		}
		if got := h.Grants(a); got != c.want {
			t.Fatalf("%q grants %q = %v, want %v", c.held, c.asked, got, c.want)
		}
	}
}

// TestAskedRefusesAWildcard pins that a service cannot ask with a "*": that
// is a programming error, and reading it as a wildcard would grant whatever
// one held permission covers.
func TestAskedRefusesAWildcard(t *testing.T) {
	for _, s := range []string{"s/a/*", "s/a/b*"} {
		if _, err := Asked(s); err == nil {
			t.Fatalf("%q was asked", s)
		}
	}
	// Even reached directly, a prefix asked permission grants nothing.
	h, err := ParsePermission("s/a/*")
	if err != nil {
		t.Fatal(err)
	}
	star, err := ParsePermission("s/a/*")
	if err != nil {
		t.Fatal(err)
	}
	if h.Grants(star) {
		t.Fatal("a prefix asked permission matched")
	}
}

// TestGrantsAny checks the set form, including that a held permission which
// breaks the grammar is an error and never a quiet miss: a reader that met
// one has read a record no host admits.
func TestGrantsAny(t *testing.T) {
	held := []string{"bsecret/release/payroll", "bstore/get/*"}
	for _, c := range []struct {
		asked string
		want  bool
	}{
		{"bsecret/release/payroll", true},
		{"bsecret/release/other", false},
		{"bstore/get/anything", true},
		{"bstore/put/anything", false},
	} {
		got, err := GrantsAny(held, c.asked)
		if err != nil {
			t.Fatalf("%q: %v", c.asked, err)
		}
		if got != c.want {
			t.Fatalf("%q = %v, want %v", c.asked, got, c.want)
		}
	}
	if _, err := GrantsAny([]string{"nonsense"}, "s/a/b"); err == nil {
		t.Fatal("a held permission that breaks the grammar was ignored")
	}
	if _, err := GrantsAny(held, "nonsense"); err == nil {
		t.Fatal("an asked permission that breaks the grammar was answered")
	}
	if got, _ := GrantsAny(nil, "s/a/b"); got {
		t.Fatal("the empty set granted something")
	}
}

// TestIntersect is attenuation: a binding keeps what its role also carries,
// so it can never widen one.
func TestIntersect(t *testing.T) {
	role := []string{"a/a/a", "b/b/b", "c/c/c"}
	for _, c := range []struct {
		keep []string
		want string
	}{
		{nil, ""},
		{[]string{"b/b/b"}, "b/b/b"},
		{[]string{"a/a/a", "c/c/c"}, "a/a/a,c/c/c"},
		{[]string{"d/d/d"}, ""},
		{[]string{"a/a/a*"}, ""},
	} {
		if got := strings.Join(Intersect(role, c.keep), ","); got != c.want {
			t.Fatalf("intersect %v = %q, want %q", c.keep, got, c.want)
		}
	}
}

// TestCheckRoleName pins the role-name grammar: an identifier, lower case and
// ASCII, so that two roles cannot be shown alike and read apart.
func TestCheckRoleName(t *testing.T) {
	for _, s := range []string{"a", "releaser", "on-call", "team_1", "v1.2", strings.Repeat("r", MaxRoleName)} {
		if err := CheckRoleName(s); err != nil {
			t.Fatalf("%q refused: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "Releaser", "on call", "röle", "admin‍", "a/b", "a:b", "a,b", "a*",
		strings.Repeat("r", MaxRoleName+1),
	} {
		if err := CheckRoleName(s); err == nil {
			t.Fatalf("%q accepted", s)
		} else if Reason(err) != "role" {
			t.Fatalf("%q refused as %q", s, Reason(err))
		}
	}
}
