package vectors

import (
	"context"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/internal/borgrec"
)

func values32(a [][32]byte) []cbor.Value {
	out := make([]cbor.Value, len(a))
	for i := range a {
		out[i] = append([]byte(nil), a[i][:]...)
	}
	return out
}

// ---- query-v1.json ----

func genQuery(s *Scenario, files map[string][]byte) error {
	o, a := hx(Org.Pub()), hx(Alice.Pub())
	gid := hx(s.Groups[0].Group[:])
	txid := displayHex(s.Commit["alice"])
	type cv struct {
		Class   string   `json:"class"`
		Members []string `json:"members"`
		Price   string   `json:"price"`
		Limit   int      `json:"limit"`
	}
	var classes []cv
	for _, c := range borgrec.Classes {
		price := "free"
		if !c.Free {
			price = "priceable"
		}
		classes = append(classes, cv{c.Name, c.Members, price, c.Limit})
	}
	q := func(kv ...string) string {
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < len(kv); i += 2 {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%q:%q", kv[i], kv[i+1])
		}
		b.WriteString("}")
		return b.String()
	}
	h := Hall
	cases := []struct{ json, class, why string }{
		{q("hall", h, "org", o), "org", ""},
		{q("hall", h, "org", o, "seq", "0"), "org-seq", ""},
		{q("hall", h, "roll", o), "roll", ""},
		{q("hall", h, "roll", o, "after", txid), "roll-after", ""},
		{q("hall", h, "org", o, "member", a), "member", ""},
		{q("hall", h, "groupsOf", o), "groups", ""},
		{q("hall", h, "groupsOf", o, "after", gid), "groups-after", ""},
		{q("hall", h, "org", o, "memberOf", a), "member-of", ""},
		{q("hall", h, "org", o, "memberOf", a, "after", gid), "member-of-after", ""},
		{q("hall", h, "org", o, "group", gid), "group", ""},
		{q("hall", h, "org", o, "group", gid, "seq", "3"), "group-seq", ""},
		{q("hall", h, "org", o, "group", gid, "epoch", "1"), "wraps", ""},
		{q("hall", h, "org", o, "group", gid, "epoch", "1", "after", txid), "wraps-after", ""},
		{q("hall", h, "org", o, "group", gid, "epoch", "1", "member", a), "wrap", ""},
		{q("hall", h, "spent", txid+".0"), "sweep", ""},
		{q("hall", h, "history", o, "chain", gid), "history", ""},
		{q("hall", h, "history", o, "chain", "org", "after", "12"), "history-after", ""},
		{`{"hall":"` + h + `","org":"00","org":"` + o + `"}`, "org", "a repeated member takes its last value"},
		{q("hall", h, "org", o, "extra", "1"), "", "a member no class defines"},
		{q("org", o), "", "hall missing"},
		{q("hall", "example_hall", "org", o), "", "a hall without its suffix"},
		{q("hall", h, "org", strings.ToUpper(o)), "", "upper-case hex"},
		{q("hall", h, "org", hx(aliasKey())), "", "a key alias"},
		{q("hall", h, "org", o, "seq", "01"), "", "a leading zero"},
		{q("hall", h, "org", o, "group", gid, "epoch", "4294967296"), "", "an epoch above 2^32 - 1"},
		{q("hall", h, "history", o, "chain", "orgs"), "", "chain names neither the organisation nor a group"},
		{q("hall", h, "org", o, "history", o, "chain", "org"), "", "a free class's members and a priceable one's together"},
		{q("hall", h, "roll", o, "after", "12"), "", "a roll cursor is a txid"},
		{q("hall", h, "spent", txid+":0"), "", "an outpoint with a colon"},
		{q("hall", h, "org", o, "group", gid, "chain", gid), "", "a member no free class defines"},
		{`{"hall":"` + h + `","org":1}`, "", "a member that is not a string"},
		{`["hall"]`, "", "not an object"},
	}
	type qv struct {
		Question string `json:"question"`
		Class    string `json:"class,omitempty"`
		Refused  bool   `json:"refused"`
		Why      string `json:"why,omitempty"`
	}
	var out []qv
	for _, c := range cases {
		got, err := borgrec.ParseQuery([]byte(c.json))
		switch {
		case c.class == "" && err == nil:
			return fmt.Errorf("query %s accepted as %s", c.json, got.Class.Name)
		case c.class != "" && err != nil:
			return fmt.Errorf("query %s refused: %v", c.json, err)
		case c.class != "" && got.Class.Name != c.class:
			return fmt.Errorf("query %s is %s, want %s", c.json, got.Class.Name, c.class)
		}
		out = append(out, qv{c.json, c.class, c.class == "", c.why})
	}
	return put(files, "query-v1.json", map[string]any{"classes": classes, "questions": out})
}

// ---- script-v1.json ----

func genScript(s *Scenario, files map[string][]byte) error {
	ctx := context.Background()
	w := Org.Wallet()
	type sv struct {
		Label  string `json:"label"`
		Script string `json:"script"`
		Claims string `json:"claims"`
	}
	var out []sv
	add := func(label string, sc *script.Script, want borgrec.Kind) error {
		got := borgrec.Classify(*sc)
		if got != want {
			return fmt.Errorf("script %q classified %q, want %q", label, got, want)
		}
		claim := string(got)
		if claim == "" {
			claim = "none"
		}
		out = append(out, sv{label, hx(*sc), claim})
		return nil
	}
	lock := func(d pushdrop.Derivation, fields [][]byte, sign bool) (*script.Script, error) {
		return d.Lock(ctx, w, "", fields, sign)
	}
	steps := []struct {
		label  string
		d      pushdrop.Derivation
		fields [][]byte
		sign   bool
		want   borgrec.Kind
	}{
		{"organisation token: tag, record, signature", borgrec.OrgDerivation, [][]byte{borgrec.TagOrg, s.OrgRecs[0]}, true, borgrec.KindOrg},
		{"group token: tag, record, signature", borgrec.GroupDerivation, [][]byte{borgrec.TagGroup, s.GroupRecs[0]}, true, borgrec.KindGroup},
		{"grant carrier output: record, signature", borgrec.GrantDerivation, [][]byte{s.GrantRecs["alice"]}, true, borgrec.KindGrant},
		{"wrap carrier output: record, signature", borgrec.RecordDerivation, [][]byte{s.WrapRecs[0]}, true, borgrec.KindWrap},
		{"funding output for wraps: the tag alone, unsigned", borgrec.RecordDerivation, [][]byte{borgrec.TagFunding}, false, borgrec.KindFunding},
		{"funding output for grants", borgrec.GrantDerivation, [][]byte{borgrec.TagFunding}, false, borgrec.KindFunding},
		{"funding output for a chain's first token", borgrec.ChainDerivation, [][]byte{borgrec.TagFunding}, false, borgrec.KindFunding},
		{"funding tag signed: not a funding output", borgrec.RecordDerivation, [][]byte{borgrec.TagFunding}, true, borgrec.KindNone},
		{"a group record as a carrier's one field: claims nothing", borgrec.RecordDerivation, [][]byte{s.GroupRecs[0]}, true, borgrec.KindNone},
		{"another application's funding tag (bb 0x02)", borgrec.RecordDerivation, [][]byte{{'b', 'b', 0x02}}, false, borgrec.KindNone},
	}
	for _, st := range steps {
		sc, err := lock(st.d, st.fields, st.sign)
		if err != nil {
			return err
		}
		if err := add(st.label, sc, st.want); err != nil {
			return err
		}
	}
	addr, _ := script.NewAddressFromPublicKey(Org.Priv.PubKey(), false)
	pk, _ := p2pkh.Lock(addr)
	if err := add("pay to public key hash", pk, borgrec.KindNone); err != nil {
		return err
	}
	trunc := script.Script(append([]byte{0x21}, append(Org.Pub(), 0xac, 0x4c, 0xff, 0xa1)...))
	if err := add("a push that declares more bytes than are present", &trunc, borgrec.KindNone); err != nil {
		return err
	}
	return put(files, "script-v1.json", map[string]any{
		"note":    "Real locking scripts from bcommon's PushDrop under the organisation's derivations. Classification reads only the key push, OP_CHECKSIG and the first push.",
		"scripts": out,
	})
}
