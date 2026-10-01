package vectors

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	aesgcm "github.com/bsv-blockchain/go-sdk/primitives/aesgcm"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/internal/borgrec"
)

// Names returns the file names in a stable order.
func Names(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for n := range files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func toJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Generate builds every vector file. It fails when a check stops holding.
func Generate() (map[string][]byte, error) {
	s, err := Build()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, f := range []func(*Scenario, map[string][]byte) error{
		genDerivation, genEpoch, genOrg, genGroup, genGrant, genWrap, genScope, genRefusal, genQuery, genScript, genTransaction,
	} {
		if err := f(s, files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func put(files map[string][]byte, name string, v any) error {
	b, err := toJSON(v)
	if err != nil {
		return err
	}
	files[name] = b
	return nil
}

func putHex(files map[string][]byte, name string, b []byte) {
	files[name] = []byte(hx(b) + "\n")
}

// ---- derivation-v1.json ----

type derivedKey struct {
	Party    string `json:"party"`
	Identity string `json:"identityKey"`
	Org      string `json:"org"`
	Group    string `json:"group"`
	Record   string `json:"record"`
	Grant    string `json:"grant"`
	Chain    string `json:"chain"`
}

func genDerivation(_ *Scenario, files map[string][]byte) error {
	short := pushdrop.Derivation{Protocol: wallet.Protocol{SecurityLevel: 1, Protocol: "borg"}, KeyID: borgrec.KeyOrg}
	refusal := short.Validate()
	if refusal == nil {
		return errors.New("go-sdk no longer refuses the protocol name \"borg\"")
	}
	for _, d := range []pushdrop.Derivation{borgrec.OrgDerivation, borgrec.GroupDerivation, borgrec.RecordDerivation, borgrec.GrantDerivation, borgrec.ChainDerivation, borgrec.FundDerivation} {
		if err := d.Validate(); err != nil {
			return err
		}
	}
	var keys []derivedKey
	forSelf := true
	for _, p := range []Party{Org, OrgOpaque, Alice, Bob, Carol} {
		dk := derivedKey{Party: p.Name, Identity: hx(p.Pub())}
		for _, x := range []struct {
			d   pushdrop.Derivation
			out *string
		}{{borgrec.OrgDerivation, &dk.Org}, {borgrec.GroupDerivation, &dk.Group}, {borgrec.RecordDerivation, &dk.Record},
			{borgrec.GrantDerivation, &dk.Grant}, {borgrec.ChainDerivation, &dk.Chain}} {
			k, err := x.d.ExpectedLockingKey(p.Priv.PubKey())
			if err != nil {
				return err
			}
			// The owner's own wallet derives the same key (forSelf).
			mine, err := p.Wallet().GetPublicKey(context.Background(), wallet.GetPublicKeyArgs{EncryptionArgs: wallet.EncryptionArgs{
				ProtocolID: x.d.Protocol, KeyID: x.d.KeyID, Counterparty: pushdrop.Anyone()}, ForSelf: &forSelf}, "")
			if err != nil {
				return err
			}
			if !bytes.Equal(mine.PublicKey.Compressed(), k.Compressed()) {
				return fmt.Errorf("%s: owner and reader derive different %s keys", p.Name, x.d.KeyID)
			}
			*x.out = hx(k.Compressed())
		}
		keys = append(keys, dk)
	}
	// Halls: one readable part, two suffixes; and one suffix drawn from
	// fixed bytes through the rejection rule.
	draw := bytes.NewReader(append([]byte{250, 251, 0, 25, 233, 234, 1, 2, 3, 4, 5, 6, 7}, make([]byte, 16)...))
	drawn, err := borgrec.NewHall("example_hall", draw)
	if err != nil {
		return err
	}
	type privKey struct {
		Party string `json:"party"`
		Priv  string `json:"privateKey"`
	}
	var privs []privKey
	for _, p := range []Party{Org, OrgOpaque, Alice, Bob, Carol} {
		privs = append(privs, privKey{p.Name, hx(p.Priv.Serialize())})
	}
	return put(files, "derivation-v1.json", map[string]any{
		"note": "Public test identities, never for real use. Every key below is derived with counterparty anyone, " +
			"so a reader computes it from the identity key alone; the owner's wallet derives the same key with forSelf.",
		"registry": map[string]any{
			"protocol": []any{1, borgrec.ProtocolName},
			"keyIds":   []string{borgrec.KeyOrg, borgrec.KeyGroup, borgrec.KeyRecord, borgrec.KeyGrant, borgrec.KeyChain, borgrec.KeyFund},
			"invoices": []string{"1-borg organisation-org", "1-borg organisation-group", "1-borg organisation-record",
				"1-borg organisation-grant", "1-borg organisation-chain", "1-borg organisation-fund"},
			"tagOrg":          hx(borgrec.TagOrg),
			"tagFunding":      hx(borgrec.TagFunding),
			"tagGroup":        hx(borgrec.TagGroup),
			"magicOrg":        hx(borgrec.MagicOrg),
			"magicGroup":      hx(borgrec.MagicGroup),
			"magicGrant":      hx(borgrec.MagicGrant),
			"magicWrap":       hx(borgrec.MagicWrap),
			"topicPrefix":     borgrec.TopicPrefix,
			"lookupService":   borgrec.LookupService,
			"grantTypeString": borgrec.GrantTypeString,
			"grantType":       borgrec.GrantTypeBase64(),
			"releaseProtocol": []any{2, borgrec.ReleaseProtocolName},
			"anyoneKey":       hx(borgrec.AnyoneKey()),
		},
		"refusedProtocol": map[string]any{"protocol": []any{1, "borg"}, "sdkError": refusal.Error()},
		"identities":      privs,
		"derivedKeys":     keys,
		"halls": map[string]any{
			"vectors":       Hall,
			"topic":         borgrec.Topic(Hall),
			"sameNameOther": "example_hall_qzxkvbmwtr",
			"drawnFromBytes": map[string]any{
				"bytes":  "fafb0019e9ea01020304050607",
				"rule":   "a byte below 234 gives the letter a + byte mod 26; 234 to 255 is skipped",
				"result": drawn,
			},
		},
	})
}

// ---- epoch-v1.json: BRC-369 reproduced, and the scenario's epoch keys ----

// The BRC-369 test vector values (BRC-369, "Test Vectors").
const (
	brcScalar  = "0ff730f7b66f7bb6626229ca20c5b4c2c47ecc32248203a14cd247ef7ed298d8"
	brcSym     = "42dcb945809f42a6f827c8336affc7a04c284dfd6e4b65c6663f6e864240ad0e"
	brcDigest  = "2013064f87db545aa20f273b3abc8bdb470ddc69349cac55b7b397585af15797"
	brcCID     = "4506d4b11347776160524138f6711c7c747d65e6bf0ea2d3f5947b98dec25a97"
	brcInvoice = "2-keyed content release-fFiOxZVLQfuY79OnswIQo8X5vER3fmI982XpY3x0s24="
	brcShared  = "873de6bc082794ae1de8390aec85b7641fb7dd88106cd0a3230468a8e9484547"
	brcPayload = "3310424203e69fe2022f968ea1bc233f6a97ab885d16965d747fdab152dc7e71675d856d6902f409d574960001d82cfc470956b1c13aa60e98fb88101cdf69797d597745c3f77c588ec5954b41fb98efd3a7b30210a3c5f9bc44777e623df365e9637c74b36efef88a7d999ae66f952cc1cef07af27ea4538a999ec5cd15038dd7a8636899298eff7a4477ee7c62fbc7f177a189749f2a17a3f7c57d89bc78ba39f6b1143ed222f3a3d1a7a71e62206123f7e998f676"
)

func unhex(s string) []byte { b, _ := hex.DecodeString(s); return b }

func genEpoch(s *Scenario, files map[string][]byte) error {
	var k [32]byte
	copy(k[:], unhex(brcScalar))
	sym, dig := borgrec.SymmetricKey(k), borgrec.Commitment(k)
	if hx(sym[:]) != brcSym || hx(dig[:]) != brcDigest {
		return errors.New("BRC-369 symmetric key or commitment no longer reproduces")
	}
	var cid [32]byte
	copy(cid[:], unhex(brcCID))
	if borgrec.ReleaseInvoice(cid, 0) != brcInvoice {
		return errors.New("BRC-369 release invoice no longer reproduces")
	}
	payload := unhex(brcPayload)
	ct := payload[102:]
	pt, err := aesgcm.AESGCMDecrypt(ct[32:len(ct)-16], unhex(brcShared), ct[:32], []byte{}, ct[len(ct)-16:])
	if err != nil || hx(pt) != brcScalar {
		return errors.New("BRC-369 release payload no longer opens with its listed shared secret")
	}
	kid := borgrec.ReleaseKeyID(cid, 0)
	if !bytes.Equal(payload[70:102], kid[:]) {
		return errors.New("BRC-369 payload key id differs from SHA-256(content id || epoch)")
	}
	if _, err := borgrec.ParseEntry(payload); err == nil {
		return errors.New("borg now accepts the BRC-369 vector's version bytes")
	}
	type ek struct {
		Epoch      int    `json:"epoch"`
		Key        string `json:"key"`
		Symmetric  string `json:"symmetricKey"`
		Commitment string `json:"commitment"`
		EpochID    string `json:"epochId"`
		BeganAtSeq uint64 `json:"beganAtSeq"`
		KeyID      string `json:"releaseKeyId"`
		Invoice    string `json:"releaseInvoice"`
	}
	var eks []ek
	for e, key := range s.EpochKeys {
		sk, c := borgrec.SymmetricKey(key), borgrec.Commitment(key)
		var seq uint64
		for _, g := range s.Groups {
			if g.BeginsEpoch() && g.Epoch == uint64(e) {
				seq = g.Seq
			}
		}
		kid := borgrec.ReleaseKeyID(s.EpochIDs[e], uint32(e))
		eks = append(eks, ek{e, hx(key[:]), hx(sk[:]), hx(c[:]), hx(s.EpochIDs[e][:]), seq, hx(kid[:]), borgrec.ReleaseInvoice(s.EpochIDs[e], uint32(e))})
	}
	// Independence: no two epochs share a key, a commitment or a symmetric key.
	for i := range eks {
		for j := i + 1; j < len(eks); j++ {
			if eks[i].Key == eks[j].Key || eks[i].Commitment == eks[j].Commitment {
				return errors.New("epoch keys repeat")
			}
		}
	}
	return put(files, "epoch-v1.json", map[string]any{
		"note":    "Epoch keys are fixed stand-ins for CSPRNG draws: SHA-256(\"borg/vector/epoch/<n>\"), each checked to be a scalar in [1, n-1].",
		"domains": map[string]string{"symmetric": borgrec.DomainSymmetric, "commitment": borgrec.DomainCommitment},
		"brc369": map[string]any{
			"source":            "BRC-369 Test Vectors: content key, release payload",
			"key":               brcScalar,
			"symmetricKey":      brcSym,
			"commitment":        brcDigest,
			"contentId":         brcCID,
			"epoch":             0,
			"releaseKeyId":      hx(kid[:]),
			"releaseInvoice":    brcInvoice,
			"payload":           brcPayload,
			"payloadVersion":    hx(payload[:4]),
			"sdkVersion":        hx(borgrec.WrapVersion),
			"payloadOpensWith":  "the vector's listed shared secret used as the AES-256-GCM key (32-byte IV, 16-byte tag): BRC-2's symmetric key, the raw x-coordinate of the child keys' shared point",
			"payloadPlaintext":  hx(pt),
			"borgAcceptsHeader": false,
		},
		"epochs": eks,
	})
}

// ---- org-v1.json ----

func genOrg(s *Scenario, files map[string][]byte) error {
	type orgVec struct {
		Label  string   `json:"label"`
		Hex    string   `json:"hex"`
		Length int      `json:"length"`
		Seq    uint64   `json:"seq"`
		Prev   string   `json:"prev"`
		Admit  []string `json:"admit"`
		Revoke []string `json:"revoke"`
		Count  uint64   `json:"count"`
		Roll   string   `json:"roll"`
		RollOf []string `json:"rollOf"`
		Opaque bool     `json:"opaque"`
		Txid   string   `json:"standInTxid,omitempty"`
	}
	mk := func(label string, o *borgrec.Org, rec []byte, rollOf [][32]byte, txid *[32]byte) (orgVec, error) {
		d, err := borgrec.DecodeOrg(rec)
		if err != nil {
			return orgVec{}, err
		}
		re, err := d.Encode()
		if err != nil || !bytes.Equal(re, rec) {
			return orgVec{}, errors.New("org record does not round-trip")
		}
		if borgrec.RollRoot(rollOf) != o.Roll {
			return orgVec{}, errors.New("roll root")
		}
		v := orgVec{Label: label, Hex: hx(rec), Length: len(rec), Seq: o.Seq, Prev: hx(o.Prev[:]), Count: o.Count,
			Roll: hx(o.Roll[:]), Opaque: o.Opaque, Admit: []string{}, Revoke: []string{}, RollOf: []string{}}
		for _, a := range o.Admit {
			v.Admit = append(v.Admit, hx(a[:]))
		}
		for _, r := range o.Revoke {
			v.Revoke = append(v.Revoke, hx(r[:]))
		}
		for _, r := range sorted32(rollOf...) {
			v.RollOf = append(v.RollOf, hx(r[:]))
		}
		if txid != nil {
			v.Txid = hx(txid[:])
		}
		return v, nil
	}
	v0, err := mk("genesis: three grants", s.Orgs[0], s.OrgRecs[0], []([32]byte){s.Commit["alice"], s.Commit["bob"], s.Commit["carol"]}, &s.OrgTxids[0])
	if err != nil {
		return err
	}
	v1, err := mk("offboard: bob's grant leaves the roll", s.Orgs[1], s.OrgRecs[1], []([32]byte){s.Commit["alice"], s.Commit["carol"]}, &s.OrgTxids[1])
	if err != nil {
		return err
	}
	ocommit := label("commitment/grant/org-opaque/alice")
	v2, err := mk("an opaque organisation's genesis", s.OpaqueOrg, s.OpaqueRaw, [][32]byte{ocommit}, nil)
	if err != nil {
		return err
	}
	// Refused organisation transitions, each one change to the admitted one.
	type otv struct {
		Label   string `json:"label"`
		To      string `json:"to"`
		Verdict string `json:"verdict"`
	}
	var refused []otv
	for _, c := range []struct {
		label  string
		mutate func(o *borgrec.Org)
		want   error
	}{
		{"seq skips", func(o *borgrec.Org) { o.Seq = 2 }, borgrec.ErrTransition},
		{"prev names another transaction", func(o *borgrec.Org) { o.Prev = label("txid/elsewhere") }, borgrec.ErrTransition},
		{"another organisation", func(o *borgrec.Org) { o.Org = OrgOpaque.Pub() }, borgrec.ErrTransition},
		{"another hall", func(o *borgrec.Org) { o.Hall = "example_hall_qzxkvbmwtr" }, borgrec.ErrTransition},
		{"opaque changed after the first token", func(o *borgrec.Org) { o.Opaque = true }, borgrec.ErrTransition},
		{"count disagrees", func(o *borgrec.Org) { o.Count = 3 }, borgrec.ErrCount},
	} {
		next := *s.Orgs[1]
		next.Revoke = append([][32]byte(nil), s.Orgs[1].Revoke...)
		c.mutate(&next)
		if err := borgrec.CheckOrgTransition(s.Orgs[0], s.OrgTxids[0], &next); !errors.Is(err, c.want) {
			return fmt.Errorf("org transition %q: got %v", c.label, err)
		}
		rec, err := next.Encode()
		if err != nil {
			return fmt.Errorf("org transition %q: %w", c.label, err)
		}
		refused = append(refused, otv{c.label, hx(rec), borgrec.Reason(c.want)})
	}
	// The member answer's inclusion context: alice's grant in the roll of
	// token 1, and bob's (revoked there) refused against it.
	kept := [][32]byte{s.Commit["alice"], s.Commit["carol"]}
	ictx, err := borgrec.ProveGrant(1, kept, s.Commit["alice"])
	if err != nil {
		return err
	}
	ctxBytes, err := ictx.Encode()
	if err != nil {
		return err
	}
	back, err := borgrec.DecodeContext(ctxBytes)
	if err != nil || !back.VerifyGrant(s.Commit["alice"], s.Orgs[1].Roll) {
		return errors.New("alice's inclusion context does not verify")
	}
	if back.VerifyGrant(s.Commit["bob"], s.Orgs[1].Roll) || back.VerifyGrant(s.Commit["alice"], s.Orgs[0].Roll) {
		return errors.New("an inclusion context verified what it should not")
	}
	putHex(files, "org-genesis.hex", s.OrgRecs[0])
	putHex(files, "org-offboard.hex", s.OrgRecs[1])
	putHex(files, "org-opaque.hex", s.OpaqueRaw)
	return put(files, "org-v1.json", map[string]any{
		"note": "Grant commitments are fixed stand-ins for carrier txids, SHA-256(\"borg/vector/commitment/grant/<member>\"); " +
			"standInTxid is the stand-in for the mined token's txid that the next token's prev names. Hashes are in hash byte order.",
		"commitments": func() map[string]string {
			out := map[string]string{}
			for n, c := range s.Commit {
				out[n] = hx(c[:])
			}
			return out
		}(),
		"records":    []orgVec{v0, v1, v2},
		"transition": map[string]any{"from": 0, "to": 1, "verdict": "admitted"},
		"refused":    refused,
		"memberContext": map[string]any{
			"grant":    "alice",
			"rollOf":   1,
			"context":  hx(ctxBytes),
			"verifies": true,
			"refused":  "bob's commitment against the same context, and alice's against token 0's roll",
		},
	})
}
