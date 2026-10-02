package vectors

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/lightwebinc/borg/borgrec"
)

// ---- group-v1.json ----

type groupVec struct {
	Label        string   `json:"label"`
	Hex          string   `json:"hex"`
	Length       int      `json:"length"`
	Seq          uint64   `json:"seq"`
	Prev         string   `json:"prev"`
	Epoch        uint64   `json:"epoch"`
	EpochSeq     uint64   `json:"epochSeq"`
	BeginsEpoch  bool     `json:"beginsEpoch"`
	EpochID      string   `json:"epochId,omitempty"`
	Commitment   string   `json:"commitment"`
	Add          []string `json:"add"`
	Remove       []string `json:"remove"`
	Count        uint64   `json:"count"`
	Members      string   `json:"members"`
	MembersAfter []string `json:"membersAfter"`
	RotateOnJoin bool     `json:"rotateOnJoin"`
	RotateEvery  uint64   `json:"rotateEvery"`
	Txid         string   `json:"standInTxid"`
}

type transitionVec struct {
	Label   string `json:"label"`
	From    string `json:"from"`
	To      string `json:"to"`
	Verdict string `json:"verdict"`
}

func keysHex(ks [][]byte) []string {
	out := []string{}
	for _, k := range ks {
		out = append(out, hx(k))
	}
	return out
}

func clone(g *borgrec.Group) *borgrec.Group {
	c := *g
	c.Add = append([][]byte(nil), g.Add...)
	c.Remove = append([][]byte(nil), g.Remove...)
	return &c
}

func genGroup(s *Scenario, files map[string][]byte) error {
	labels := []string{
		"create: alice and bob, epoch 0",
		"add carol: same epoch, no rotation",
		"remove bob: epoch 1, a new key",
		"scheduled rotation: epoch 2, no member change, rotate every 30 days from here",
	}
	names := []string{"group-create.hex", "group-add.hex", "group-remove.hex", "group-rotate.hex"}
	set := map[string]bool{}
	var vecs []groupVec
	for i, g := range s.Groups {
		d, err := borgrec.DecodeGroup(s.GroupRecs[i])
		if err != nil {
			return err
		}
		re, err := d.Encode()
		if err != nil || !bytes.Equal(re, s.GroupRecs[i]) {
			return errors.New("group record does not round-trip")
		}
		if err := borgrec.Fold(set, g); err != nil {
			return err
		}
		var after [][]byte
		for k := range set {
			after = append(after, []byte(k))
		}
		v := groupVec{Label: labels[i], Hex: hx(s.GroupRecs[i]), Length: len(s.GroupRecs[i]), Seq: g.Seq, Prev: hx(g.Prev[:]),
			Epoch: g.Epoch, EpochSeq: g.EpochSeq, BeginsEpoch: g.BeginsEpoch(), Commitment: hx(g.Commitment[:]),
			Add: keysHex(g.Add), Remove: keysHex(g.Remove), Count: g.Count, Members: hx(g.Members[:]),
			MembersAfter: keysHex(sortedBytes(after)), RotateOnJoin: g.RotateOnJoin, RotateEvery: g.RotateEvery,
			Txid: hx(s.GroupTxids[i][:])}
		if g.BeginsEpoch() {
			id := borgrec.EpochID(s.GroupRecs[i])
			v.EpochID = hx(id[:])
		}
		vecs = append(vecs, v)
		putHex(files, names[i], s.GroupRecs[i])
	}

	// Transitions: the scenario's, each admitted, then refusals, each a
	// one-field change to an admitted successor that breaks one rule.
	var tv []transitionVec
	for i := 1; i < len(s.Groups); i++ {
		tv = append(tv, transitionVec{Label: labels[i], From: fmt.Sprint(i - 1), To: fmt.Sprint(i), Verdict: "admitted"})
	}
	type bad struct {
		label  string
		from   int
		mutate func(g *borgrec.Group)
		want   error
	}
	g0, g1, g2 := s.Groups[0], s.Groups[1], s.Groups[2]
	joinRotating := clone(g0)
	joinRotating.RotateOnJoin = true
	cases := []bad{
		{"removal without a new epoch", 1, func(g *borgrec.Group) {
			g.Epoch, g.EpochSeq, g.Commitment = g1.Epoch, g1.EpochSeq, g1.Commitment
		}, borgrec.ErrRotation},
		{"new epoch reusing the key", 1, func(g *borgrec.Group) { g.Commitment = g1.Commitment }, borgrec.ErrRotation},
		{"new epoch whose epochSeq is not its seq", 1, func(g *borgrec.Group) { g.EpochSeq = 0 }, borgrec.ErrRotation},
		{"same epoch, a different key", 0, func(g *borgrec.Group) { g.Commitment = borgrec.Commitment(label("epoch/stray")) }, borgrec.ErrRotation},
		{"epoch skips", 1, func(g *borgrec.Group) { g.Epoch = 2 }, borgrec.ErrTransition},
		{"seq skips", 1, func(g *borgrec.Group) { g.Seq = 3; g.EpochSeq = 3 }, borgrec.ErrTransition},
		{"prev names another transaction", 1, func(g *borgrec.Group) { g.Prev = label("txid/elsewhere") }, borgrec.ErrTransition},
		{"another group", 1, func(g *borgrec.Group) { g.Group[0] ^= 1 }, borgrec.ErrTransition},
		{"count disagrees", 1, func(g *borgrec.Group) { g.Count = 3 }, borgrec.ErrCount},
	}
	for _, c := range cases {
		prev, prevTxid := s.Groups[c.from], s.GroupTxids[c.from]
		next := clone(s.Groups[c.from+1])
		c.mutate(next)
		if err := borgrec.CheckGroupTransition(prev, prevTxid, next); !errors.Is(err, c.want) {
			return fmt.Errorf("transition %q: got %v, want %v", c.label, err, c.want)
		}
		rec, err := next.Encode()
		if err != nil {
			return fmt.Errorf("transition %q: %w", c.label, err)
		}
		tv = append(tv, transitionVec{Label: c.label, From: fmt.Sprint(c.from), To: hx(rec), Verdict: borgrec.Reason(c.want)})
	}
	// A join while the predecessor asks for rotation on join.
	{
		jrRec, err := joinRotating.Encode()
		if err != nil {
			return err
		}
		next := clone(g1)
		if err := borgrec.CheckGroupTransition(joinRotating, s.GroupTxids[0], next); !errors.Is(err, borgrec.ErrRotation) {
			return fmt.Errorf("rotateOnJoin: got %v", err)
		}
		tv = append(tv, transitionVec{Label: "add while the predecessor sets rotateOnJoin, without a new epoch",
			From: hx(jrRec), To: "1", Verdict: "rotation"})
	}
	// A join after rotateOnJoin, with its new epoch: admitted.
	{
		jrRec, err := joinRotating.Encode()
		if err != nil {
			return err
		}
		next := clone(g1)
		next.Epoch, next.EpochSeq, next.Commitment = 1, 1, borgrec.Commitment(label("epoch/join"))
		if err := borgrec.CheckGroupTransition(joinRotating, s.GroupTxids[0], next); err != nil {
			return fmt.Errorf("rotateOnJoin with rotation: %v", err)
		}
		rec, err := next.Encode()
		if err != nil {
			return err
		}
		tv = append(tv, transitionVec{Label: "add while the predecessor sets rotateOnJoin, with a new epoch",
			From: hx(jrRec), To: hx(rec), Verdict: "admitted"})
	}
	// What admission cannot see and the fold refuses: an index reports these
	// as members-mismatch, and a reader fails closed.
	type fv struct {
		Label   string   `json:"label"`
		Before  []string `json:"membersBefore"`
		Token   string   `json:"token"`
		Verdict string   `json:"verdict"`
	}
	var folds []fv
	before := sortedKeys(Alice, Bob, Carol)
	for _, c := range []struct {
		label  string
		mutate func(g *borgrec.Group)
	}{
		{"removes a key that is not a member", func(g *borgrec.Group) { g.Remove = sortedKeys(Dave) }},
		{"adds a key that is already a member", func(g *borgrec.Group) { g.Remove = nil; g.Add = sortedKeys(Alice); g.Count = 3 }},
		{"a members root that is not the fold's", func(g *borgrec.Group) { g.Members = borgrec.MembersRoot(sortedKeys(Alice)) }},
	} {
		bad := clone(g2)
		c.mutate(bad)
		set := map[string]bool{}
		for _, k := range before {
			set[string(k)] = true
		}
		if err := borgrec.Fold(set, bad); err == nil {
			return fmt.Errorf("fold accepted: %s", c.label)
		}
		rec, err := bad.Encode()
		if err != nil {
			return fmt.Errorf("fold %q: %w", c.label, err)
		}
		folds = append(folds, fv{c.label, keysHex(before), hx(rec), "members-mismatch"})
	}
	// A key removed earlier and added again (spec section 4.2): the index
	// reports the token rejoin-unrotated unless it begins an epoch.
	type rv struct {
		Label   string   `json:"label"`
		Removed []string `json:"removedBefore"`
		From    string   `json:"from"`
		Token   string   `json:"token"`
		Verdict string   `json:"verdict"`
	}
	var rejoins []rv
	{
		g3, g3Txid := s.Groups[3], s.GroupTxids[3]
		removed := map[string]bool{}
		for _, g := range s.Groups {
			for _, k := range g.Remove {
				removed[string(k)] = true
			}
		}
		var removedHex [][]byte
		for k := range removed {
			removedHex = append(removedHex, []byte(k))
		}
		set := map[string]bool{}
		for _, g := range s.Groups {
			if err := borgrec.Fold(set, g); err != nil {
				return err
			}
		}
		set[string(Bob.Pub())] = true
		var mem [][]byte
		for k := range set {
			mem = append(mem, []byte(k))
		}
		same := clone(g3)
		same.Seq, same.Prev, same.Add, same.Remove = g3.Seq+1, g3Txid, sortedKeys(Bob), nil
		same.Count, same.Members, same.Created = g3.Count+1, borgrec.MembersRoot(sortedBytes(mem)), g3.Created+60
		rotated := clone(same)
		rotated.Epoch, rotated.EpochSeq, rotated.Commitment = g3.Epoch+1, rotated.Seq, borgrec.Commitment(label("epoch/rejoin"))
		for _, c := range []struct {
			label   string
			g       *borgrec.Group
			verdict string
		}{
			{"bob re-added in the same epoch", same, "rejoin-unrotated"},
			{"bob re-added with a new epoch", rotated, "consistent"},
		} {
			if err := borgrec.CheckGroupTransition(g3, g3Txid, c.g); err != nil {
				return fmt.Errorf("rejoin %q: admission: %v", c.label, err)
			}
			if got := borgrec.RejoinUnrotated(removed, c.g); got != (c.verdict == "rejoin-unrotated") {
				return fmt.Errorf("rejoin %q: got %v", c.label, got)
			}
			rec, err := c.g.Encode()
			if err != nil {
				return err
			}
			rejoins = append(rejoins, rv{c.label, keysHex(sortedBytes(removedHex)), "3", hx(rec), c.verdict})
		}
	}
	return put(files, "group-v1.json", map[string]any{
		"note": "standInTxid is the stand-in for each mined token's txid; the next token's prev names it. " +
			"A transition's from and to are record indexes into records, or a record's hex where the case changes it. " +
			"Each refusal is admission's verdict with the predecessor read from the parent the token's BEEF carries.",
		"rejoin": map[string]any{
			"rule":  "a token that adds a key any earlier token of the group removed, and does not begin an epoch, is rejoin-unrotated; both cases are admitted, the status is the index's and a reader's",
			"cases": rejoins,
		},
		"groupId":     hx(s.Groups[0].Group[:]),
		"records":     vecs,
		"transitions": tv,
		"fold": map[string]any{
			"rule":  "members after a token = members before, less remove, plus add; MembersRoot of the result equals members; an index's and a reader's check, never admission's",
			"cases": folds,
		},
	})
}

func sortedBytes(a [][]byte) [][]byte {
	out := append([][]byte(nil), a...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && bytes.Compare(out[j-1], out[j]) > 0; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ---- grant-v1.json ----

type grantVec struct {
	Label      string            `json:"label"`
	Org        string            `json:"org"`
	Subject    string            `json:"subject"`
	Hex        string            `json:"hex"`
	Length     int               `json:"length"`
	Serial     string            `json:"serial"`
	Revocation string            `json:"revocationOutpoint"`
	Values     map[string]string `json:"values"`
	FieldKeys  map[string]string `json:"fieldRevelationKeys"`
	FieldIVs   map[string]string `json:"fieldIVs"`
	KeyringIVs map[string]string `json:"keyringIVs"`
	Fields     map[string]string `json:"fields"`
	Keyring    map[string]string `json:"keyring"`
	Reveal     map[string]string `json:"reveal,omitempty"`
	Preimage   string            `json:"preimage"`
	Signature  string            `json:"signature"`
	Membership bool              `json:"membership"`
	Admin      bool              `json:"admin"`
}

func hexMap(m map[string][]byte) map[string]string {
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = hx(v)
	}
	return out
}

func hexMap32(m map[string][32]byte) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = hx(v[:])
	}
	return out
}

func genGrant(s *Scenario, files map[string][]byte) error {
	ctx := context.Background()
	mk := func(label string, org, member Party, g *borgrec.Grant, rec []byte, vout uint32, reveal bool) (grantVec, error) {
		d, err := borgrec.DecodeGrant(rec)
		if err != nil {
			return grantVec{}, err
		}
		re, err := d.Encode()
		if err != nil || !bytes.Equal(re, rec) {
			return grantVec{}, errors.New("grant record does not round-trip")
		}
		if err := d.CheckCertificate(ctx); err != nil {
			return grantVec{}, err
		}
		if err := d.CheckRevocation(d.RevTxid, d.RevVout); err != nil {
			return grantVec{}, err
		}
		// The member opens its own fields through its wallet (go-sdk's
		// certificates.DecryptFields), whatever the reveal.
		open, err := d.OpenFields(ctx, member.Wallet())
		if err != nil {
			return grantVec{}, fmt.Errorf("%s cannot open its grant: %w", member.Name, err)
		}
		values := GrantValues[member.Name]
		for n, v := range values {
			if open[n] != v {
				return grantVec{}, fmt.Errorf("%s field %s opens to %q", member.Name, n, open[n])
			}
		}
		// Another member cannot.
		other := Bob
		if member.Name == "bob" {
			other = Carol
		}
		if _, err := d.OpenFields(ctx, other.Wallet()); err == nil {
			return grantVec{}, errors.New("another member opened a grant's fields")
		}
		revealed, err := d.CheckReveal()
		if err != nil {
			return grantVec{}, err
		}
		if reveal {
			for n, v := range values {
				if revealed[n] != v {
					return grantVec{}, errors.New("reveal disagrees with the keyring")
				}
			}
		} else if revealed != nil {
			return grantVec{}, errors.New("an opaque grant revealed something")
		}
		spec := grantSpec(org, member, vout, values, reveal)
		pre, err := d.Preimage()
		if err != nil {
			return grantVec{}, err
		}
		return grantVec{Label: label, Org: hx(d.Org), Subject: hx(d.Subject), Hex: hx(rec), Length: len(rec),
			Serial: hx(d.Serial[:]), Revocation: displayHex(d.RevTxid) + "." + fmt.Sprint(d.RevVout), Values: values,
			FieldKeys: hexMap32(spec.FieldKeys), FieldIVs: hexMap32(spec.FieldIVs), KeyringIVs: hexMap32(spec.KeyringIVs),
			Fields: hexMap(d.Fields), Keyring: hexMap(d.Keyring), Reveal: hexMap(d.Reveal), Preimage: hx(pre),
			Signature: hx(d.Signature), Membership: borgrec.IsMembership(values["scope"]),
			Admin: func() bool { t, _ := borgrec.ParseScope(values["scope"]); return borgrec.Grants(t, borgrec.VerbAdmin) }()}, nil
	}
	var vecs []grantVec
	for i, m := range members {
		v, err := mk(m.Name+": revealed to everyone", Org, m, s.Grants[m.Name], s.GrantRecs[m.Name], uint32(i), true)
		if err != nil {
			return err
		}
		vecs = append(vecs, v)
		putHex(files, "grant-"+m.Name+".hex", s.GrantRecs[m.Name])
	}
	v, err := mk("alice, from an opaque organisation: no reveal", OrgOpaque, Alice, s.OpaqueGrant, s.OpaqueRec, 0, false)
	if err != nil {
		return err
	}
	vecs = append(vecs, v)
	putHex(files, "grant-opaque.hex", s.OpaqueRec)
	return put(files, "grant-v1.json", map[string]any{
		"note": "Field revelation keys and IVs are fixed stand-ins for CSPRNG draws, SHA-256 of labels. " +
			"preimage is the BRC-52 binary form without the signature (go-sdk ToBinary(false)); the signature is the organisation's " +
			"under [2, \"certificate signature\"], key id \"<type> <serial>\" in base64, verified with counterparty the organisation. " +
			"Each member opens its own fields through its wallet; another member cannot. The revocation outpoint is a stand-in " +
			"funding outpoint, written as BRC-100's OutpointString.",
		"certificateType": borgrec.GrantTypeBase64(),
		"grants":          vecs,
	})
}

// ---- wrap-v1.json ----

type entryVec struct {
	Recipient string `json:"recipient"`
	Member    string `json:"member"`
	IV        string `json:"iv"`
	Wrap      string `json:"wrap"`
}

type openVec struct {
	Party  string `json:"party"`
	Opens  bool   `json:"opens"`
	Key    string `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type wrapVec struct {
	Label   string     `json:"label"`
	Hex     string     `json:"hex"`
	Length  int        `json:"length"`
	Epoch   uint64     `json:"epoch"`
	EpochID string     `json:"epochId"`
	KeyID   string     `json:"releaseKeyId"`
	Entries []entryVec `json:"entries"`
	Opens   []openVec  `json:"opens"`
}

func nameOf(k []byte) string {
	for _, p := range []Party{Org, Alice, Bob, Carol} {
		if bytes.Equal(p.Pub(), k) {
			return p.Name
		}
	}
	return "?"
}

func genWrap(s *Scenario, files map[string][]byte) error {
	ctx := context.Background()
	labels := []string{"epoch 0 to alice and bob, after create", "epoch 0 to carol, after the add",
		"epoch 1 to alice and carol, after bob's removal", "epoch 2 to alice and carol, after the scheduled rotation"}
	names := []string{"wrap-create.hex", "wrap-add.hex", "wrap-remove.hex", "wrap-rotate.hex"}
	var vecs []wrapVec
	for i, w := range s.Wraps {
		d, err := borgrec.DecodeWrap(s.WrapRecs[i])
		if err != nil {
			return err
		}
		re, err := d.Encode()
		if err != nil || !bytes.Equal(re, s.WrapRecs[i]) {
			return errors.New("wrap record does not round-trip")
		}
		kid := borgrec.ReleaseKeyID(w.EpochID, uint32(w.Epoch))
		v := wrapVec{Label: labels[i], Hex: hx(s.WrapRecs[i]), Length: len(s.WrapRecs[i]), Epoch: w.Epoch,
			EpochID: hx(w.EpochID[:]), KeyID: hx(kid[:])}
		for _, e := range w.Entries {
			r := e[37:70]
			iv := s.WrapIVs[i][nameOf(r)]
			v.Entries = append(v.Entries, entryVec{Recipient: hx(r), Member: nameOf(r), IV: hx(iv[:]), Wrap: hx(e)})
		}
		for _, p := range []Party{Alice, Bob, Carol} {
			k, ok, err := s.unwrapFor(ctx, w, p)
			ov := openVec{Party: p.Name, Opens: ok}
			if ok {
				if k != s.EpochKeys[w.Epoch] {
					return errors.New("unwrapped the wrong key")
				}
				ov.Key = hx(k[:])
			} else {
				if err == nil {
					return errors.New("a failed unwrap reported no error")
				}
				ov.Reason = borgrec.Reason(err) + ": no entry names this member, and another member's entry does not decrypt"
			}
			v.Opens = append(v.Opens, ov)
		}
		vecs = append(vecs, v)
		putHex(files, names[i], s.WrapRecs[i])
	}
	// The exit criterion, as checks: bob holds epoch 0's key, cannot open
	// epoch 1 or 2, and his epoch 0 key does not meet their commitments.
	for _, e := range []int{1, 2} {
		if _, ok, _ := s.unwrapFor(ctx, s.Wraps[e+1], Bob); ok {
			return fmt.Errorf("bob opened epoch %d", e)
		}
		if borgrec.Commitment(s.EpochKeys[0]) == s.epochCommitment(e) {
			return errors.New("epoch 0's key meets a later commitment")
		}
	}
	// Check 1 against every wrap, and a wrap held against another epoch's token.
	startRec := func(e uint64) []byte {
		for i, g := range s.Groups {
			if g.BeginsEpoch() && g.Epoch == e {
				return s.GroupRecs[i]
			}
		}
		return nil
	}
	for _, w := range s.Wraps {
		if _, err := borgrec.CheckEpochToken(startRec(w.Epoch), w); err != nil {
			return fmt.Errorf("epoch token: %w", err)
		}
	}
	if _, err := borgrec.CheckEpochToken(startRec(0), s.Wraps[2]); borgrec.Reason(err) != "token" {
		return fmt.Errorf("an epoch 1 wrap passed check 1 against epoch 0's token: %v", err)
	}
	// Replay: an entry moved to another epoch's record breaks the key id rule.
	moved := *s.Wraps[2]
	moved.Entries = [][]byte{s.Wraps[0].Entries[0]}
	if _, err := moved.Encode(); !errors.Is(err, borgrec.ErrEntry) {
		return fmt.Errorf("an epoch 0 entry was accepted in an epoch 1 record: %v", err)
	}
	return put(files, "wrap-v1.json", map[string]any{
		"note": "Each entry is BRC-369 section 5.2's release in BRC-78 serialization: version 42421033, the organisation, the member, " +
			"the key id SHA-256(epochId || epoch as 4 bytes big-endian), then BRC-2 ciphertext (32-byte IV, the 32-byte key, 16-byte tag) " +
			"under [2, \"keyed content release\"]. IVs are fixed stand-ins, SHA-256(\"borg/vector/iv/wrap/<epoch>/<member>\"). " +
			"opens records what each member's own wallet recovers, checked against the epoch's commitment (BRC-369 section 5.3 check 2).",
		"wraps": vecs,
		"removal": map[string]any{
			"removed":            "bob",
			"atSeq":              2,
			"holds":              []int{0},
			"cannotOpen":         []int{1, 2},
			"replayAcrossEpochs": "refused: entry (key id)",
			"checkLabels":        []string{"token", "entry", "undecryptable", "commitment"},
			"wrongToken":         "the epoch 1 wrap held against epoch 0's token: token",
		},
	})
}
