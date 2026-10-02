package vectors

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/lightwebinc/borg/borgrec"
)

// ---- scope-v1.json ----

func genScope(_ *Scenario, files map[string][]byte) error {
	type sv struct {
		Scope      string `json:"scope"`
		Parses     bool   `json:"parses"`
		Membership bool   `json:"membership"`
		Admin      bool   `json:"admin"`
		Why        string `json:"why"`
	}
	cases := []struct{ s, why string }{
		{"member", "the one token a membership needs"},
		{"member,admin", "two defined tokens"},
		{"admin", "parses, but is not a membership: member is absent"},
		{"member,release:3f2a", "an unknown verb authorises nothing and denies nothing else"},
		{"member:eng", "a defined verb with a qualifier this version does not define is denied, never read as the bare verb"},
		{"member,member:eng", "the qualified copy still denies member"},
		{"member,thread:abc.def_1", "BRC-169's qualifier characters"},
		{"*", "the whole-scope wildcard is refused for a membership grant"},
		{"", "empty"},
		{"Member", "upper case"},
		{"member,", "an empty token"},
		{"member admin", "a space"},
		{"member:", "an empty qualifier"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "a verb of 33 characters"},
		{"member:" + string(bytes.Repeat([]byte{'q'}, 65)), "a qualifier of 65 characters"},
	}
	var out []sv
	for _, c := range cases {
		t, err := borgrec.ParseScope(c.s)
		v := sv{Scope: c.s, Parses: err == nil, Why: c.why}
		if err == nil {
			v.Membership, v.Admin = borgrec.Grants(t, borgrec.VerbMember), borgrec.Grants(t, borgrec.VerbAdmin)
		}
		out = append(out, v)
	}
	type tv struct {
		Value string `json:"value"`
		Ok    bool   `json:"ok"`
	}
	var exp []tv
	for _, e := range []string{"2028-01-01T00:00:00Z", "9999-12-31T23:59:59Z", "2028-02-30T00:00:00Z", "2028-01-01T00:00:00+00:00",
		"2028-01-01 00:00:00Z", "2028-01-01T00:00:60Z", "2028-1-01T00:00:00Z", "1969-12-31T23:59:59Z"} {
		_, err := borgrec.ParseExpiry(e)
		exp = append(exp, tv{e, err == nil})
	}
	// A role is decrypted bytes, which need not be UTF-8; JSON cannot carry
	// such bytes, so each role is also given as hex.
	type rv struct {
		Value string `json:"value"`
		Hex   string `json:"hex"`
		Ok    bool   `json:"ok"`
	}
	var roles []rv
	for _, r := range []string{"engineer", "team lead", "", string(bytes.Repeat([]byte{'r'}, 65)), "\xff"} {
		roles = append(roles, rv{r, hx([]byte(r)), borgrec.CheckRole(r) == nil})
	}
	return put(files, "scope-v1.json", map[string]any{
		"grammar": "BRC-169 section 9.2, with \"*\" refused; defined verbs: member, admin",
		"scopes":  out,
		"expiry":  exp,
		"roles":   roles,
	})
}

// ---- refusal-v1.json ----

type refusalVec struct {
	Record string `json:"record"`
	Label  string `json:"label"`
	Hex    string `json:"hex"`
	Reason string `json:"reason"`
	Stage  string `json:"stage"`
}

func asMap(rec []byte) cbor.Map {
	v, err := cbor.DecodeValue(rec)
	if err != nil {
		panic(err)
	}
	return v.(cbor.Map)
}

func setKey(m cbor.Map, k uint64, v cbor.Value) cbor.Map {
	out := make(cbor.Map, 0, len(m)+1)
	done := false
	for _, p := range m {
		if p.Key == k {
			if v != nil {
				out = append(out, cbor.Pair{Key: k, Val: v})
			}
			done = true
			continue
		}
		out = append(out, p)
	}
	if !done && v != nil {
		out = append(out, cbor.Pair{Key: k, Val: v})
	}
	return out
}

type deleted struct{}

func mutate(rec []byte, k uint64, v cbor.Value) []byte {
	m := asMap(rec)
	if _, ok := v.(deleted); ok {
		m = setKey(m, k, nil)
	} else {
		m = setKey(m, k, v)
	}
	b, err := cbor.Encode(m)
	if err != nil {
		panic(err)
	}
	return b
}

func aliasKey() []byte {
	p := ec.S256().P
	x := new(big.Int).Add(p, big.NewInt(1)).Bytes()
	return append([]byte{0x02}, x...)
}

func genRefusal(s *Scenario, files map[string][]byte) error {
	ctx := context.Background()
	decoders := map[string]func([]byte) error{
		"org":   func(b []byte) error { _, err := borgrec.DecodeOrg(b); return err },
		"group": func(b []byte) error { _, err := borgrec.DecodeGroup(b); return err },
		"grant": func(b []byte) error { _, err := borgrec.DecodeGrant(b); return err },
		"wrap":  func(b []byte) error { _, err := borgrec.DecodeWrap(b); return err },
	}
	org, grp, grant, wrap := s.OrgRecs[0], s.GroupRecs[2], s.GrantRecs["alice"], s.WrapRecs[2]
	big := bytes.Repeat([]byte{0}, 9000)
	type c struct {
		rec, label string
		b          []byte
		want       string
	}
	unsorted := [][32]byte{s.Orgs[0].Admit[1], s.Orgs[0].Admit[0]}
	cases := []c{
		{"org", "over the bound", mutate(org, 40, big), "too-large"},
		{"org", "trailing byte", append(append([]byte(nil), org...), 0x00), "cbor"},
		{"org", "an array, not a map", []byte{0x81, 0x00}, "cbor"},
		{"org", "a text key", func() []byte {
			b, _ := cbor.Encode(append(asMap(org), cbor.Pair{Key: "x", Val: uint64(1)}))
			return b
		}(), "key-type"},
		{"org", "the group magic", mutate(org, 0, borgrec.MagicGroup), "magic"},
		{"org", "hall missing", mutate(org, 1, deleted{}), "missing"},
		{"org", "hall as bytes", mutate(org, 1, []byte(Hall)), "type"},
		{"org", "hall without a suffix", mutate(org, 1, "example_hall"), "hall"},
		{"org", "organisation key alias (02 || p+1)", mutate(org, 2, aliasKey()), "identity"},
		{"org", "empty name", mutate(org, 5, ""), "name"},
		{"org", "admit not ascending", mutate(org, 6, values32(unsorted)), "list"},
		{"org", "count above the roll bound", mutate(org, 8, uint64(borgrec.MaxRoll+1)), "range"},
		{"org", "created 0", mutate(org, 11, uint64(0)), "range"},
		{"org", "seq 0 with a prev", mutate(org, 4, bytes.Repeat([]byte{1}, 32)), "chain"},
		{"org", "seq 0 whose count is not its admit list", mutate(org, 8, uint64(2)), "count"},
		{"group", "add names the organisation", mutate(s.GroupRecs[1], 10, []cbor.Value{Org.Pub()}), "members"},
		{"group", "add names the anyone key", mutate(s.GroupRecs[1], 10, []cbor.Value{borgrec.AnyoneKey()}), "members"},
		{"group", "epochSeq after seq", mutate(grp, 8, uint64(3)), "chain"},
		{"group", "group id of 15 bytes", mutate(grp, 3, bytes.Repeat([]byte{7}, 15)), "range"},
		{"group", "epoch above 2^32 - 1", mutate(grp, 7, uint64(1)<<32), "range"},
		{"group", "count above the member bound", mutate(grp, 12, uint64(borgrec.MaxMembers+1)), "range"},
		{"group", "rotateOnJoin not a boolean", mutate(grp, 14, uint64(0)), "type"},
		{"group", "65 removals", mutate(grp, 11, make([]cbor.Value, 65)), "list"},
		{"grant", "subject is the organisation", mutate(grant, 3, Org.Pub()), "members"},
		{"grant", "a fourth field", func() []byte {
			m := asMap(grant)
			f := asMap(mustEncode(getKey(m, 6)))
			f = append(f, cbor.Pair{Key: "team", Val: bytes.Repeat([]byte{1}, 60)})
			return mutate(grant, 6, f)
		}(), "fields"},
		{"grant", "a keyring entry of 79 bytes", func() []byte {
			m := asMap(grant)
			k := asMap(mustEncode(getKey(m, 8)))
			k = setTextKey(k, "role", bytes.Repeat([]byte{1}, 79))
			return mutate(grant, 8, k)
		}(), "fields"},
		{"grant", "a high-S signature", mutate(grant, 7, highS(s.Grants["alice"].Signature)), "signature-form"},
		{"grant", "revocation vout above 2^32 - 1", mutate(grant, 5, []cbor.Value{bytes.Repeat([]byte{2}, 32), uint64(1) << 32}), "range"},
		{"grant", "a high-S signature and a 79-byte keyring entry: key 7 is decided first", mutate(mutate(grant, 7, highS(s.Grants["alice"].Signature)), 8,
			setTextKey(asMap(mustEncode(getKey(asMap(grant), 8))), "role", bytes.Repeat([]byte{1}, 79))), "signature-form"},
		{"grant", "a short field ciphertext and no signature: key 6 is decided first", mutate(mutate(grant, 7, deleted{}), 6,
			setTextKey(asMap(mustEncode(getKey(asMap(grant), 6))), "role", bytes.Repeat([]byte{1}, 48))), "fields"},
		{"grant", "a short field ciphertext and a keyring that is not a map", mutate(mutate(grant, 8, uint64(1)), 6,
			setTextKey(asMap(mustEncode(getKey(asMap(grant), 6))), "role", bytes.Repeat([]byte{1}, 48))), "fields"},
		{"grant", "no revocation outpoint", mutate(grant, 5, deleted{}), "missing"},
		{"grant", "revocation of three elements", mutate(grant, 5, []cbor.Value{bytes.Repeat([]byte{2}, 32), uint64(0), uint64(0)}), "type"},
		{"grant", "the organisation is the anyone key", mutate(grant, 2, borgrec.AnyoneKey()), "identity"},
		{"org", "the organisation is the anyone key", mutate(org, 2, borgrec.AnyoneKey()), "identity"},
		{"wrap", "no entries", mutate(wrap, 6, []cbor.Value{}), "list"},
		{"wrap", "17 entries", mutate(wrap, 6, func() []cbor.Value {
			out := make([]cbor.Value, 17)
			for i := range out {
				out[i] = s.Wraps[2].Entries[0]
			}
			return out
		}()), "list"},
		{"wrap", "entries not ascending", mutate(wrap, 6, []cbor.Value{s.Wraps[2].Entries[1], s.Wraps[2].Entries[0]}), "entry"},
		{"wrap", "an entry from epoch 0 (key id)", mutate(wrap, 6, []cbor.Value{s.Wraps[0].Entries[0]}), "entry"},
		{"wrap", "the record's epoch changed (key id)", mutate(wrap, 4, uint64(2)), "entry"},
		{"wrap", "another sender", mutate(wrap, 2, Alice.Pub()), "entry"},
		{"wrap", "the version bytes of BRC-369's vector", mutate(wrap, 6, []cbor.Value{withVersion(s.Wraps[2].Entries[0], []byte{0x33, 0x10, 0x42, 0x42})}), "entry"},
		{"wrap", "an entry of 181 bytes", mutate(wrap, 6, []cbor.Value{s.Wraps[2].Entries[0][:181]}), "entry"},
	}
	var out []refusalVec
	for _, x := range cases {
		err := decoders[x.rec](x.b)
		if got := borgrec.Reason(err); got != x.want {
			return fmt.Errorf("refusal %s %q: got %q (%v), want %q", x.rec, x.label, got, err, x.want)
		}
		out = append(out, refusalVec{x.rec, x.label, hx(x.b), x.want, "record"})
	}

	// Content rules: the record decodes, the rule after the carrier's refuses.
	tamper := mutate(grant, 6, func() cbor.Map {
		f := asMap(mustEncode(getKey(asMap(grant), 6)))
		r := append([]byte(nil), getText(f, "role").([]byte)...)
		r[40] ^= 1
		return setTextKey(f, "role", r)
	}())
	g, err := borgrec.DecodeGrant(tamper)
	if err != nil {
		return err
	}
	if e := g.CheckCertificate(ctx); borgrec.Reason(e) != "grant-signature" {
		return fmt.Errorf("tampered field: %v", e)
	}
	out = append(out, refusalVec{"grant", "a field ciphertext changed after signing", hx(tamper), "grant-signature", "content"})
	wrongReveal := mutate(grant, 9, setTextKey(asMap(mustEncode(getKey(asMap(grant), 9))), "role", bytes.Repeat([]byte{9}, 32)))
	if g, err = borgrec.DecodeGrant(wrongReveal); err != nil {
		return err
	}
	if _, e := g.CheckReveal(); borgrec.Reason(e) != "grant-reveal" {
		return fmt.Errorf("wrong reveal: %v", e)
	}
	out = append(out, refusalVec{"grant", "a revealed key that does not open its field", hx(wrongReveal), "grant-reveal", "content"})
	if g, err = borgrec.DecodeGrant(grant); err != nil {
		return err
	}
	other := label("tree/other")
	if e := g.CheckRevocation(other, 0); borgrec.Reason(e) != "grant-revocation" {
		return fmt.Errorf("revocation: %v", e)
	}
	out = append(out, refusalVec{"grant", "carried by a carrier whose input spends " + displayHex(other) + ".0",
		hx(grant), "grant-revocation", "content"})

	// Unknown keys are preserved, not refused.
	withUnknown := mutate(org, 30, "a later version's field")
	o, err := borgrec.DecodeOrg(withUnknown)
	if err != nil {
		return err
	}
	re, err := o.Encode()
	if err != nil || !bytes.Equal(re, withUnknown) {
		return errors.New("an unknown key was not preserved")
	}
	return put(files, "refusal-v1.json", map[string]any{
		"note": "Each case is a valid scenario record with one change. stage record: the record's own rules refuse it, in the order of " +
			"spec section 3.1; stage content: it decodes, and the content rule named refuses it after the carrier rules.",
		"refusals":       out,
		"unknownKeyKept": map[string]string{"record": "org", "hex": hx(withUnknown), "key": "30"},
	})
}

func getKey(m cbor.Map, k uint64) cbor.Value {
	for _, p := range m {
		if p.Key == k {
			return p.Val
		}
	}
	return nil
}

func getText(m cbor.Map, name string) cbor.Value {
	for _, p := range m {
		if p.Key == name {
			return p.Val
		}
	}
	return nil
}

func setTextKey(m cbor.Map, name string, v cbor.Value) cbor.Map {
	out := make(cbor.Map, 0, len(m))
	for _, p := range m {
		if p.Key == name {
			out = append(out, cbor.Pair{Key: name, Val: v})
			continue
		}
		out = append(out, p)
	}
	return out
}

// highS rewrites a DER signature with S replaced by n - S.
func highS(der []byte) []byte {
	sig, err := ec.ParseDERSignature(der)
	if err != nil {
		panic(err)
	}
	n := ec.S256().N
	s := new(big.Int).Sub(n, sig.S)
	r := sig.R.Bytes()
	if r[0]&0x80 != 0 {
		r = append([]byte{0}, r...)
	}
	sb := s.Bytes()
	if sb[0]&0x80 != 0 {
		sb = append([]byte{0}, sb...)
	}
	out := []byte{0x30, byte(4 + len(r) + len(sb)), 0x02, byte(len(r))}
	out = append(out, r...)
	out = append(out, 0x02, byte(len(sb)))
	return append(out, sb...)
}

func withVersion(entry, v []byte) []byte {
	out := append([]byte(nil), entry...)
	copy(out, v)
	return out
}

func mustEncode(v cbor.Value) []byte {
	b, err := cbor.Encode(v)
	if err != nil {
		panic(err)
	}
	return b
}
