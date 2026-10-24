package module

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/borgrec"
)

// The exit criterion of the application, shown through the module's lookup
// answers (exit.test.ts): a removal from a group rotates the epoch, every
// host refuses a removal that does not, and the removed member cannot open
// the next epoch's key, while the members who remain open it with nothing
// but what the host answers. The member's checks are spec section 6.4's,
// in its order: token, entry, undecryptable, commitment.

// ask is a question's outputs, as transactions.
func ask(t *testing.T, ls *Service, q qm) []*transaction.Transaction {
	t.Helper()
	var out []*transaction.Transaction
	for _, h := range hydrated(t, ls, q) {
		out = append(out, subjectOf(t, h.Beef))
	}
	return out
}

type opened struct {
	epoch   uint64
	key     [32]byte
	refused string
}

func recordOf(t *testing.T, tx *transaction.Transaction) []byte {
	t.Helper()
	p, ok := pushdrop.FirstPush(*tx.Outputs[0].LockingScript)
	if !ok {
		t.Fatal("no first push")
	}
	return p
}

// openEpoch is what a member does to read its group's current epoch key
// from a host: the latest token, the token that began its epoch (checked
// against the wrap's epoch id), its own wrap, the decryption, the
// commitment.
func openEpoch(t *testing.T, ls *Service, org *orgSim, groupHex string, member *party) opened {
	t.Helper()
	latest := ask(t, ls, qm{"hall": hall, "org": org.hex(), "group": groupHex})
	if len(latest) != 1 {
		t.Fatalf("%d latest tokens", len(latest))
	}
	head, err := borgrec.ReadToken(latest[0])
	if err != nil {
		t.Fatal(err)
	}
	began := ask(t, ls, qm{"hall": hall, "org": org.hex(), "group": groupHex, "seq": fmt.Sprint(head.Group.EpochSeq)})
	tok, err := borgrec.ReadToken(began[0])
	if err != nil {
		t.Fatal(err)
	}
	epoch := head.Group.Epoch
	wraps := ask(t, ls, qm{"hall": hall, "org": org.hex(), "group": groupHex, "epoch": fmt.Sprint(epoch), "member": member.hex})
	refused := "entry"
	for _, w := range wraps {
		rec, err := borgrec.DecodeWrap(recordOf(t, w))
		if err != nil {
			t.Fatal(err)
		}
		g, err := borgrec.CheckEpochToken(tok.Record, rec)
		if err != nil {
			refused = borgrec.Reason(err)
			continue
		}
		i := -1
		for j, r := range rec.Recipients() {
			if bytes.Equal(r, member.id) {
				i = j
			}
		}
		if i < 0 {
			continue
		}
		orgKey := org.party.key.PubKey()
		k, err := borgrec.Unwrap(context.Background(), member.w, rec.Entries[i], orgKey, rec.EpochID, uint32(rec.Epoch), g.Commitment) //nolint:gosec // an epoch is at most 2^32 - 1
		if err != nil {
			refused = borgrec.Reason(err)
			continue
		}
		return opened{epoch: epoch, key: k}
	}
	return opened{epoch: epoch, refused: refused}
}

func TestRemovalRotatesAndTheRemovedMemberCannotOpen(t *testing.T) {
	chain := newTestChain(90000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "exit org", chain, orgOpts{})
	alice, bob, carol := newParty("exit alice"), newParty("exit bob"), newParty("exit carol")
	grants := []*grantMade{org.grant(alice), org.grant(bob), org.grant(carol)}
	for _, g := range grants {
		outs(t, s.submitTx(g.tx), 0)
	}
	outs(t, s.submit(org.orgToken(grantsOf(grants...), nil, tokenOpts{}).beef), 0)
	group := org.group("private", []*party{alice, bob, carol}, false)
	outs(t, s.submit(group.tip.beef), 0)
	for _, w := range group.wraps(nil, 0) {
		outs(t, s.submitTx(w), 0)
	}

	// Epoch 0: all three open it, and it is the key the organization
	// sampled.
	k0 := group.epochs[0].key
	for _, p := range []*party{alice, bob, carol} {
		if got := openEpoch(t, s.ls, org, group.idHex(), p); got != (opened{epoch: 0, key: k0}) {
			t.Fatalf("%s: %+v", p.label, got)
		}
	}

	// A removal that keeps the epoch is refused by the host, raising:
	// nothing recorded.
	tip, members, n := group.tip, group.members, len(group.history)
	unrotated := group.change(changeOpts{remove: []*party{bob}, keepEpoch: true})
	outs(t, s.submit(unrotated.beef))
	if s.host.count("borg_refused_total", map[string]string{"reason": "rotation"}) != 1 || s.storage.isApplied(idOf(unrotated.tx), topic) {
		t.Fatal("refused rotation, nothing recorded")
	}
	group.tip, group.members, group.history = tip, members, group.history[:n]

	// The removal as the tooling builds it: a new epoch, a fresh key, wraps
	// to those who remain.
	removal := group.change(changeOpts{remove: []*party{bob}})
	if removal.grp.Epoch != 1 || removal.grp.EpochSeq != removal.grp.Seq || removal.grp.Commitment == borgrec.Commitment(k0) {
		t.Fatalf("%+v", removal.grp)
	}
	outs(t, s.submit(removal.beef), 0)
	for _, w := range group.wraps(nil, 0) {
		outs(t, s.submitTx(w), 0)
	}
	if tokenStatus(t, s.ls, idOf(removal.tx)) != borgrec.StatusConsistent {
		t.Fatal("consistent")
	}

	// What the host answers: bob is in no group and no wrap of epoch 1
	// names him.
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": bob.hex}), nil, "member-of bob")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": alice.hex}), ids(removal.tx), "member-of alice")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "group": group.idHex(), "epoch": "1", "member": bob.hex}), nil, "wrap 1 bob")

	// Alice and carol open epoch 1; bob is refused at the entry check.
	k1 := group.epochs[1].key
	for _, p := range []*party{alice, carol} {
		if got := openEpoch(t, s.ls, org, group.idHex(), p); got != (opened{epoch: 1, key: k1}) {
			t.Fatalf("%s: %+v", p.label, got)
		}
	}
	if got := openEpoch(t, s.ls, org, group.idHex(), bob); got != (opened{epoch: 1, refused: "entry"}) {
		t.Fatalf("bob: %+v", got)
	}

	// Bob tries every entry of every wrap of epoch 1 with his own wallet:
	// none opens.
	all := ask(t, s.ls, qm{"hall": hall, "org": org.hex(), "group": group.idHex(), "epoch": "1"})
	if len(all) == 0 {
		t.Fatal("no wraps of epoch 1")
	}
	opens := 0
	for _, w := range all {
		rec, err := borgrec.DecodeWrap(recordOf(t, w))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range rec.Entries {
			_, err := bob.w.Decrypt(context.Background(), wallet.DecryptArgs{EncryptionArgs: wallet.EncryptionArgs{ProtocolID: borgrec.ReleaseProtocol,
				KeyID: base64.StdEncoding.EncodeToString(entry[70:102]), Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: org.party.key.PubKey()}},
				Ciphertext: entry[102:]}, "")
			if err == nil {
				opens++
			}
		}
	}
	if opens != 0 {
		t.Fatalf("bob opened %d entries", opens)
	}
	// Epoch 0 stays open to him: rotation is not retroactive (spec section
	// 8).
	if old := ask(t, s.ls, qm{"hall": hall, "org": org.hex(), "group": group.idHex(), "epoch": "0", "member": bob.hex}); len(old) != 1 {
		t.Fatalf("%d wraps of epoch 0 name bob", len(old))
	}

	// A later rotation: bob still cannot open it, the others can.
	rot := group.change(changeOpts{rotate: true})
	outs(t, s.submit(rot.beef), 0)
	for _, w := range group.wraps(nil, 0) {
		outs(t, s.submitTx(w), 0)
	}
	if got := openEpoch(t, s.ls, org, group.idHex(), carol); got != (opened{epoch: 2, key: group.epochs[2].key}) {
		t.Fatalf("carol: %+v", got)
	}
	if got := openEpoch(t, s.ls, org, group.idHex(), bob); got != (opened{epoch: 2, refused: "entry"}) {
		t.Fatalf("bob: %+v", got)
	}
}

func TestAMembersChecksRefuseAWrapMadeWrong(t *testing.T) {
	chain := newTestChain(90500)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "careless org", chain, orgOpts{})
	alice, carol, dave := newParty("careless alice"), newParty("careless carol"), newParty("careless dave")
	group := org.group("careless", []*party{alice, carol, dave}, false)
	outs(t, s.submit(group.tip.beef), 0)
	ek := group.epochs[0]
	seal := func(entries [][]byte, id [32]byte) {
		rec := &borgrec.WrapRecord{Hall: hall, Org: org.party.id, Group: group.id, Epoch: 0, EpochID: id, Entries: entries}
		outs(t, s.submitTx(mintCarrier(t, org.recordTree, org.nextRecordVout(), org.party, borgrec.RecordDerivation, encode(t, rec), lockTime0)), 0)
	}
	// alice: another key than the one committed to.
	seal([][]byte{wrapEntry(t, org.party, alice.id, ek.epochID, 0, epochKey(t))}, ek.epochID)
	// carol: her key, its ciphertext damaged.
	damaged := wrapEntry(t, org.party, carol.id, ek.epochID, 0, ek.key)
	damaged[120] ^= 1
	seal([][]byte{damaged}, ek.epochID)
	// dave: a wrap naming another epoch id than the token that began the
	// epoch.
	var other [32]byte
	copy(other[:], sha("another epoch id"))
	seal([][]byte{wrapEntry(t, org.party, dave.id, other, 0, ek.key)}, other)
	for _, w := range []struct {
		p    *party
		want string
	}{{alice, "commitment"}, {carol, "undecryptable"}, {dave, "token"}} {
		if got := openEpoch(t, s.ls, org, group.idHex(), w.p); got != (opened{epoch: 0, refused: w.want}) {
			t.Fatalf("%s: %+v, want %s", w.p.label, got, w.want)
		}
	}
}
