package vectors

import (
	"context"
	"fmt"
	"sort"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/borg/borgrec"
)

// Scenario is the story every record vector tells: an organization grants
// three members, makes a group of two, adds the third without rotating,
// removes one (the epoch rotates and the removed member cannot open it),
// rotates on schedule, and offboards the removed member from the roll.
type Scenario struct {
	Grants      map[string]*borgrec.Grant // by member name
	GrantRecs   map[string][]byte
	Commit      map[string][32]byte // stand-in grant commitments (carrier txids)
	OpaqueGrant *borgrec.Grant
	OpaqueRec   []byte

	Orgs      []*borgrec.Org
	OrgRecs   [][]byte
	OrgTxids  [][32]byte // stand-ins for each org token's mined txid
	OpaqueOrg *borgrec.Org
	OpaqueRaw []byte

	Groups     []*borgrec.Group
	GroupRecs  [][]byte
	GroupTxids [][32]byte
	EpochKeys  [][32]byte // by epoch
	EpochIDs   [][32]byte // by epoch

	Wraps    []*borgrec.WrapRecord
	WrapRecs [][]byte
	WrapIVs  []map[string][32]byte
}

// OrgOpaque is the second test organization, whose roll is opaque.
var OrgOpaque = party("org-opaque", 0x55)

var members = []Party{Alice, Bob, Carol}

// emptyPolicy is the policy root of an organization that has published no
// policy record: the RFC 6962 root of nothing, never the zero value.
var emptyPolicy = borgrec.PolicyRoot(nil)

func byName(name string) Party {
	for _, p := range append(members, Org, OrgOpaque) {
		if p.Name == name {
			return p
		}
	}
	panic(name)
}

// grantSpec builds a grant spec from labels.
func grantSpec(org, member Party, vout uint32, values map[string]string, reveal bool) borgrec.GrantSpec {
	s := borgrec.GrantSpec{
		Hall: Hall, Subject: member.Priv.PubKey(), Serial: label("serial/%s/%s", org.Name, member.Name),
		RevTxid: label("tree/grants/%s", org.Name), RevVout: vout, Values: values, Reveal: reveal,
		FieldKeys: map[string][32]byte{}, FieldIVs: map[string][32]byte{}, KeyringIVs: map[string][32]byte{},
	}
	for _, n := range borgrec.GrantFieldNames {
		s.FieldKeys[n] = label("fieldkey/%s/%s/%s", org.Name, member.Name, n)
		s.FieldIVs[n] = label("iv/field/%s/%s/%s", org.Name, member.Name, n)
		s.KeyringIVs[n] = label("iv/keyring/%s/%s/%s", org.Name, member.Name, n)
	}
	return s
}

// GrantValues are the members' grant fields in the clear.
var GrantValues = map[string]map[string]string{
	"alice": {"expiry": "2028-01-01T00:00:00Z", "role": "engineer", "scope": "member"},
	"bob":   {"expiry": "2028-01-01T00:00:00Z", "role": "team lead", "scope": "member,admin"},
	"carol": {"expiry": "2027-07-01T00:00:00Z", "role": "contractor", "scope": "member"},
}

func sortedKeys(ps ...Party) [][]byte {
	out := make([][]byte, len(ps))
	for i, p := range ps {
		out[i] = p.Pub()
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i]) < string(out[j]) })
	return out
}

func sorted32(a ...[32]byte) [][32]byte {
	out := append([][32]byte(nil), a...)
	sort.Slice(out, func(i, j int) bool { return string(out[i][:]) < string(out[j][:]) })
	return out
}

// Build makes the scenario and checks it as it goes.
func Build() (*Scenario, error) {
	ctx := context.Background()
	s := &Scenario{Grants: map[string]*borgrec.Grant{}, GrantRecs: map[string][]byte{}, Commit: map[string][32]byte{}}

	for i, m := range members {
		g, err := borgrec.BuildGrant(ctx, Org.Priv, grantSpec(Org, m, uint32(i), GrantValues[m.Name], true))
		if err != nil {
			return nil, fmt.Errorf("grant %s: %w", m.Name, err)
		}
		rec, err := g.Encode()
		if err != nil {
			return nil, err
		}
		s.Grants[m.Name], s.GrantRecs[m.Name] = g, rec
		s.Commit[m.Name] = label("commitment/grant/%s", m.Name)
	}
	og, err := borgrec.BuildGrant(ctx, OrgOpaque.Priv, grantSpec(OrgOpaque, Alice, 0, GrantValues["alice"], false))
	if err != nil {
		return nil, err
	}
	if s.OpaqueRec, err = og.Encode(); err != nil {
		return nil, err
	}
	s.OpaqueGrant = og

	// The roll: three grants admitted, then bob's revoked.
	all := sorted32(s.Commit["alice"], s.Commit["bob"], s.Commit["carol"])
	kept := sorted32(s.Commit["alice"], s.Commit["carol"])
	s.Orgs = []*borgrec.Org{
		{Hall: Hall, Org: Org.Pub(), Seq: 0, Name: "Example Organization", Admit: all,
			Count: 3, Roll: borgrec.RollRoot(all), Created: Created, Policy: emptyPolicy},
		{Hall: Hall, Org: Org.Pub(), Seq: 1, Prev: label("txid/org/0"), Name: "Example Organization",
			Revoke: [][32]byte{s.Commit["bob"]}, Count: 2, Roll: borgrec.RollRoot(kept), Created: Created + 7200,
			Policy: emptyPolicy},
	}
	for i, o := range s.Orgs {
		rec, err := o.Encode()
		if err != nil {
			return nil, fmt.Errorf("org %d: %w", i, err)
		}
		s.OrgRecs = append(s.OrgRecs, rec)
		s.OrgTxids = append(s.OrgTxids, label("txid/org/%d", i))
	}
	if err := borgrec.CheckOrgTransition(s.Orgs[0], s.OrgTxids[0], s.Orgs[1]); err != nil {
		return nil, fmt.Errorf("org transition: %w", err)
	}
	ocommit := label("commitment/grant/org-opaque/alice")
	s.OpaqueOrg = &borgrec.Org{Hall: Hall, Org: OrgOpaque.Pub(), Name: "Opaque Organization",
		Admit: [][32]byte{ocommit}, Count: 1, Roll: borgrec.RollRoot([][32]byte{ocommit}), Opaque: true,
		Created: Created, Policy: emptyPolicy}
	if s.OpaqueRaw, err = s.OpaqueOrg.Encode(); err != nil {
		return nil, err
	}

	// The epoch keys: fixed stand-ins for CSPRNG draws, each checked.
	for e := 0; e < 3; e++ {
		k := label("epoch/%d", e)
		if err := borgrec.CheckScalar(k); err != nil {
			return nil, err
		}
		s.EpochKeys = append(s.EpochKeys, k)
	}

	gid := GroupID()
	base := func(seq uint64) *borgrec.Group {
		g := &borgrec.Group{Hall: Hall, Org: Org.Pub(), Group: gid, Seq: seq, Name: "Engineering",
			Created: Created + 600*seq + 600}
		if seq > 0 {
			g.Prev = label("txid/group/%d", seq-1)
		}
		return g
	}
	g0 := base(0)
	g0.Add, g0.Count = sortedKeys(Alice, Bob), 2
	g0.Members, g0.Commitment = borgrec.MembersRoot(g0.Add), borgrec.Commitment(s.EpochKeys[0])
	g1 := base(1)
	g1.Epoch, g1.EpochSeq, g1.Commitment = 0, 0, g0.Commitment
	g1.Add, g1.Count = sortedKeys(Carol), 3
	g1.Members = borgrec.MembersRoot(sortedKeys(Alice, Bob, Carol))
	g2 := base(2)
	g2.Epoch, g2.EpochSeq, g2.Commitment = 1, 2, borgrec.Commitment(s.EpochKeys[1])
	g2.Remove, g2.Count = sortedKeys(Bob), 2
	g2.Members = borgrec.MembersRoot(sortedKeys(Alice, Carol))
	g3 := base(3)
	g3.Epoch, g3.EpochSeq, g3.Commitment = 2, 3, borgrec.Commitment(s.EpochKeys[2])
	g3.Count, g3.Members, g3.RotateEvery = 2, g2.Members, 2592000
	s.Groups = []*borgrec.Group{g0, g1, g2, g3}
	set := map[string]bool{}
	for i, g := range s.Groups {
		rec, err := g.Encode()
		if err != nil {
			return nil, fmt.Errorf("group %d: %w", i, err)
		}
		s.GroupRecs = append(s.GroupRecs, rec)
		s.GroupTxids = append(s.GroupTxids, label("txid/group/%d", i))
		if i > 0 {
			if err := borgrec.CheckGroupTransition(s.Groups[i-1], s.GroupTxids[i-1], g); err != nil {
				return nil, fmt.Errorf("group transition %d: %w", i, err)
			}
		}
		if err := borgrec.Fold(set, g); err != nil {
			return nil, fmt.Errorf("group fold %d: %w", i, err)
		}
		if g.BeginsEpoch() {
			s.EpochIDs = append(s.EpochIDs, borgrec.EpochID(rec))
		}
	}

	// The wraps, each published after the token that justifies it.
	type plan struct {
		epoch int
		to    []Party
	}
	for _, p := range []plan{{0, []Party{Alice, Bob}}, {0, []Party{Carol}}, {1, []Party{Alice, Carol}}, {2, []Party{Alice, Carol}}} {
		w := &borgrec.WrapRecord{Hall: Hall, Org: Org.Pub(), Group: gid, Epoch: uint64(p.epoch), EpochID: s.EpochIDs[p.epoch]}
		to := append([]Party(nil), p.to...)
		sort.Slice(to, func(i, j int) bool { return string(to[i].Pub()) < string(to[j].Pub()) })
		ivs := map[string][32]byte{}
		for _, m := range to {
			iv := label("iv/wrap/%d/%s", p.epoch, m.Name)
			ivs[m.Name] = iv
			e, err := borgrec.WrapWithIV(Org.Priv, m.Priv.PubKey(), w.EpochID, uint32(p.epoch), s.EpochKeys[p.epoch], iv)
			if err != nil {
				return nil, err
			}
			w.Entries = append(w.Entries, e)
		}
		rec, err := w.Encode()
		if err != nil {
			return nil, fmt.Errorf("wrap: %w", err)
		}
		s.Wraps, s.WrapRecs, s.WrapIVs = append(s.Wraps, w), append(s.WrapRecs, rec), append(s.WrapIVs, ivs)
	}
	return s, nil
}

// epochCommitment returns the commitment of epoch e from the token that
// began it.
func (s *Scenario) epochCommitment(e int) [32]byte {
	for _, g := range s.Groups {
		if g.BeginsEpoch() && g.Epoch == uint64(e) {
			return g.Commitment
		}
	}
	panic(e)
}

// unwrapFor tries every entry of a wrap record with p's wallet and returns
// the key it opens, or the error of the last try.
func (s *Scenario) unwrapFor(ctx context.Context, w *borgrec.WrapRecord, p Party) ([32]byte, bool, error) {
	org, _ := ec.PublicKeyFromBytes(w.Org)
	var last error
	for _, e := range w.Entries {
		k, err := borgrec.Unwrap(ctx, p.Wallet(), e, org, w.EpochID, uint32(w.Epoch), s.epochCommitment(int(w.Epoch)))
		if err == nil {
			return k, true, nil
		}
		last = err
	}
	return [32]byte{}, false, last
}
