package borgrec

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/cbor"
)

const testHall = "example_hall_vkqzmtrwxp"

func orgKey() []byte { return key(0x51).PubKey().Compressed() }

func definition() *Policy {
	return &Policy{
		Hall:        testHall,
		Org:         orgKey(),
		Kind:        PolicyRole,
		Role:        "releaser",
		Permissions: []string{"bsecret/release/payroll", "bstore/get/*"},
		Created:     1,
	}
}

func binding(to []byte) *Policy {
	return &Policy{
		Hall:    testHall,
		Org:     orgKey(),
		Kind:    PolicyBinding,
		Role:    "releaser",
		To:      to,
		Expiry:  "2030-01-02T03:04:05Z",
		Created: 1,
	}
}

func groupID() []byte { return bytes.Repeat([]byte{0x11}, GroupIDLen) }

// TestPolicyRoundTrip encodes each kind and reads it back unchanged.
func TestPolicyRoundTrip(t *testing.T) {
	for _, p := range []*Policy{
		definition(),
		binding(groupID()),
		binding(key(0x52).PubKey().Compressed()),
		{Hall: testHall, Org: orgKey(), Kind: PolicyBinding, Role: "a", To: groupID(),
			Permissions: []string{"bstore/get/photos/*"}, Created: 1},
	} {
		b, err := p.Encode()
		if err != nil {
			t.Fatalf("encode %q: %v", p.Role, err)
		}
		got, err := DecodePolicy(b)
		if err != nil {
			t.Fatalf("decode %q: %v", p.Role, err)
		}
		got.Extra = nil
		if got.Hall != p.Hall || !bytes.Equal(got.Org, p.Org) || got.Kind != p.Kind ||
			got.Role != p.Role || got.Expiry != p.Expiry || got.Created != p.Created ||
			!bytes.Equal(got.To, p.To) || strings.Join(got.Permissions, ",") != strings.Join(p.Permissions, ",") {
			t.Fatalf("round trip changed the record: %+v vs %+v", got, p)
		}
		again, err := got.Encode()
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("re-encode is not byte identical: %v", err)
		}
	}
}

// TestPolicyNullKeysPresent pins that an absent subject and expiry are
// written as CBOR null rather than left out, so every key this version
// defines is always present and a reader never guesses.
func TestPolicyNullKeysPresent(t *testing.T) {
	b, err := definition().Encode()
	if err != nil {
		t.Fatal(err)
	}
	m, err := cbor.DecodeValue(b)
	if err != nil {
		t.Fatal(err)
	}
	mm, ok := m.(cbor.Map)
	if !ok {
		t.Fatal("not a map")
	}
	if len(mm) != pkLast+1 {
		t.Fatalf("record has %d keys, want %d", len(mm), pkLast+1)
	}
	for _, k := range []uint64{PKTo, PKExpiry} {
		v, ok := mm.Get(k)
		if !ok {
			t.Fatalf("key %d is absent, not null", k)
		}
		if v != nil {
			t.Fatalf("key %d is %v, want null", k, v)
		}
	}
	// A null where this version expects a value is a type refusal, not a
	// silent zero.
	raw := cbor.Map{
		{Key: uint64(PKMagic), Val: MagicPolicy}, {Key: uint64(PKHall), Val: testHall},
		{Key: uint64(PKOrg), Val: orgKey()}, {Key: uint64(PKKind), Val: PolicyRole},
		{Key: uint64(PKRole), Val: nil}, {Key: uint64(PKPermissions), Val: []cbor.Value{}},
		{Key: uint64(PKTo), Val: nil}, {Key: uint64(PKExpiry), Val: nil},
		{Key: uint64(PKCreated), Val: uint64(1)},
	}
	enc, err := cbor.Encode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(enc); Reason(err) != "type" {
		t.Fatalf("a null role decoded as %v (%s)", err, Reason(err))
	}
}

// TestPolicyRefusals walks every rule of the record, each case breaking one.
func TestPolicyRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		mutate func(*Policy)
	}{
		{"hall", "hall", func(p *Policy) { p.Hall = "no_suffix" }},
		{"org not a key", "identity", func(p *Policy) { p.Org = []byte{1, 2, 3} }},
		{"org is anyone", "identity", func(p *Policy) { p.Org = AnyoneKey() }},
		{"kind too high", "range", func(p *Policy) { p.Kind = 2 }},
		{"role empty", "role", func(p *Policy) { p.Role = "" }},
		{"role upper case", "role", func(p *Policy) { p.Role = "Releaser" }},
		{"role too long", "role", func(p *Policy) { p.Role = strings.Repeat("a", MaxRoleName+1) }},
		{"role space", "role", func(p *Policy) { p.Role = "a b" }},
		{"permissions not ascending", "list", func(p *Policy) {
			p.Permissions = []string{"b/b/b", "a/a/a"}
		}},
		{"permissions duplicated", "list", func(p *Policy) {
			p.Permissions = []string{"a/a/a", "a/a/a"}
		}},
		{"too many permissions", "list", func(p *Policy) {
			p.Permissions = manyPermissions(MaxPermissions + 1)
		}},
		{"permission grammar", "permission", func(p *Policy) { p.Permissions = []string{"a/a"} }},
		{"permission wildcard service", "permission", func(p *Policy) { p.Permissions = []string{"*/a/a"} }},
		{"created zero", "range", func(p *Policy) { p.Created = 0 }},
		{"definition with a subject", "binding", func(p *Policy) { p.To = groupID() }},
		{"definition with an expiry", "binding", func(p *Policy) { p.Expiry = "2030-01-02T03:04:05Z" }},
	} {
		p := definition()
		c.mutate(p)
		_, err := p.Encode()
		if err == nil {
			t.Fatalf("%s: encoded", c.name)
		}
		if got := Reason(err); got != c.reason {
			t.Fatalf("%s: reason %q, want %q (%v)", c.name, got, c.reason, err)
		}
	}
	for _, c := range []struct {
		name   string
		reason string
		mutate func(*Policy)
	}{
		{"binding without a subject", "binding", func(p *Policy) { p.To = nil }},
		{"subject of the wrong length", "identity", func(p *Policy) { p.To = []byte{1, 2, 3} }},
		{"subject not a key", "identity", func(p *Policy) { p.To = bytes.Repeat([]byte{9}, 33) }},
		{"subject is anyone", "identity", func(p *Policy) { p.To = AnyoneKey() }},
		{"subject is the organisation", "identity", func(p *Policy) { p.To = orgKey() }},
		{"expiry spelling", "expiry", func(p *Policy) { p.Expiry = "2030-01-02 03:04:05" }},
		{"expiry not a date", "expiry", func(p *Policy) { p.Expiry = "2030-02-30T03:04:05Z" }},
	} {
		p := binding(groupID())
		c.mutate(p)
		_, err := p.Encode()
		if err == nil {
			t.Fatalf("%s: encoded", c.name)
		}
		if got := Reason(err); got != c.reason {
			t.Fatalf("%s: reason %q, want %q (%v)", c.name, got, c.reason, err)
		}
	}
}

func manyPermissions(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "s/a/" + string(rune('a'+i/26)) + string(rune('a'+i%26))
	}
	return out
}

// TestPolicyDecodeRefusals covers the refusals a decoder meets that an
// encoder cannot produce: the magic, a missing key, a wrong type and the
// record bound.
func TestPolicyDecodeRefusals(t *testing.T) {
	good, err := definition().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(append(good, 0x00)); Reason(err) != "cbor" {
		t.Fatalf("trailing byte: %s", Reason(err))
	}
	wrong := bytes.Replace(good, MagicPolicy, []byte{'b', 'o', 'p', 0x02}, 1)
	if _, err := DecodePolicy(wrong); Reason(err) != "magic" {
		t.Fatalf("version 2 magic: %s", Reason(err))
	}
	wrong = bytes.Replace(good, MagicPolicy, MagicGrant, 1)
	if _, err := DecodePolicy(wrong); Reason(err) != "magic" {
		t.Fatalf("a grant's magic: %s", Reason(err))
	}
	// A missing key, built by hand.
	m := cbor.Map{
		{Key: uint64(PKMagic), Val: MagicPolicy}, {Key: uint64(PKHall), Val: testHall},
		{Key: uint64(PKOrg), Val: orgKey()}, {Key: uint64(PKKind), Val: PolicyRole},
		{Key: uint64(PKRole), Val: "releaser"}, {Key: uint64(PKPermissions), Val: []cbor.Value{}},
		{Key: uint64(PKTo), Val: nil}, {Key: uint64(PKCreated), Val: uint64(1)},
	}
	enc, err := cbor.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(enc); Reason(err) != "missing" {
		t.Fatalf("missing expiry: %s", Reason(err))
	}
	// A permission that is not text.
	m = append(m, cbor.Pair{Key: uint64(PKExpiry), Val: nil})
	for i := range m {
		if m[i].Key == uint64(PKPermissions) {
			m[i].Val = []cbor.Value{uint64(7)}
		}
	}
	if enc, err = cbor.Encode(m); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(enc); Reason(err) != "list" {
		t.Fatalf("permission of the wrong type: %s", Reason(err))
	}
}

// TestPolicyUnknownKeyPreserved pins the rule every record follows: a key
// above the last this version defines is kept verbatim and ignored, so a
// later version's record survives a reader that rewrites it.
func TestPolicyUnknownKeyPreserved(t *testing.T) {
	p := definition()
	p.Extra = cbor.Map{{Key: uint64(pkLast + 1), Val: "later"}}
	b, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePolicy(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extra) != 1 || got.Extra[0].Key != uint64(pkLast+1) || got.Extra[0].Val != "later" {
		t.Fatalf("unknown key not preserved: %+v", got.Extra)
	}
	again, err := got.Encode()
	if err != nil || !bytes.Equal(again, b) {
		t.Fatalf("re-encode with an unknown key changed the bytes: %v", err)
	}
}

// TestPolicyRecordFitsItsBound checks the bound against the worst case the
// grammar allows: MaxPermissions permissions of full length.
func TestPolicyRecordFitsItsBound(t *testing.T) {
	long := "s" + strings.Repeat("x", MaxService-1) + "/" + strings.Repeat("a", MaxAction) + "/"
	perms := make([]string, MaxPermissions)
	for i := range perms {
		// Distinct and ascending, each the longest the grammar allows.
		tail := strings.Repeat("r", MaxResource-2) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		perms[i] = long + tail + "*"
	}
	p := binding(key(0x52).PubKey().Compressed())
	p.Role = strings.Repeat("r", MaxRoleName)
	p.Permissions = perms
	b, err := p.Encode()
	if err != nil {
		t.Fatalf("the worst case does not encode: %v", err)
	}
	if len(b) > MaxPolicyRecord {
		t.Fatalf("worst case is %d bytes, over the bound %d", len(b), MaxPolicyRecord)
	}
	t.Logf("worst case: %d bytes of %d", len(b), MaxPolicyRecord)
	if _, err := DecodePolicy(b); err != nil {
		t.Fatalf("the worst case does not decode: %v", err)
	}
}

// TestEffectiveFoldRules covers what the vectors cannot show in one pass:
// the union deduplicates and sorts, an attenuation cannot widen, and a
// binding to a group the subject is not in contributes nothing.
func TestEffectiveFoldRules(t *testing.T) {
	gid := [GroupIDLen]byte{1}
	other := [GroupIDLen]byte{2}
	def := func(role string, perms ...string) *Policy {
		return &Policy{Hall: testHall, Org: orgKey(), Kind: PolicyRole, Role: role, Permissions: perms, Created: 1}
	}
	bind := func(role string, to []byte, expiry string, only ...string) *Policy {
		return &Policy{Hall: testHall, Org: orgKey(), Kind: PolicyBinding, Role: role, To: to,
			Expiry: expiry, Permissions: only, Created: 1}
	}
	k := key(0x52).PubKey().Compressed()
	now, err := ParseExpiry("2026-06-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	set := []*Policy{
		def("reader", "s/get/a", "s/get/b"),
		def("writer", "s/put/a", "s/get/a"),
		bind("reader", gid[:], ""),
		bind("writer", k, ""),
		bind("reader", other[:], ""),
		bind("writer", k, "", "s/put/a", "s/delete/a"),
	}
	s := Subject{Key: k, Member: true, Groups: [][GroupIDLen]byte{gid}}
	got := Effective(s, set, now)
	want := []string{"s/get/a", "s/get/b", "s/put/a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("effective %v, want %v", got, want)
	}
	// An attenuation naming a permission the definition does not carry adds
	// nothing: the binding above names s/delete/a and the role does not.
	for _, p := range got {
		if p == "s/delete/a" {
			t.Fatal("an attenuation widened a role")
		}
	}
	// A subject that is not a current member holds nothing at all.
	s.Member = false
	if len(Effective(s, set, now)) != 0 {
		t.Fatal("a subject with no current grant held permissions")
	}
	// An expired binding contributes nothing; the same binding before its
	// expiry does.
	s = Subject{Key: k, Member: true}
	past := []*Policy{def("reader", "s/get/a"), bind("reader", k, "2020-01-01T00:00:00Z")}
	if len(Effective(s, past, now)) != 0 {
		t.Fatal("an expired binding granted something")
	}
	future := []*Policy{def("reader", "s/get/a"), bind("reader", k, "2030-01-01T00:00:00Z")}
	if len(Effective(s, future, now)) != 1 {
		t.Fatal("a binding that has not expired granted nothing")
	}
	// Within the skew an expiry just passed is still current.
	exp := now.Add(-ExpirySkew / 2).UTC().Format(ExpiryLayout)
	if len(Effective(s, []*Policy{def("reader", "s/get/a"), bind("reader", k, exp)}, now)) != 1 {
		t.Fatal("a binding inside the clock skew was dropped")
	}
}
