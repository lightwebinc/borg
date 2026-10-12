package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/internal/testchain"
)

var regexpLatest = regexp.MustCompile(`(?m)^latest  seq \d+ ([0-9a-f]{64})`)

// copyDir copies a home, as a second device of one organisation would hold
// it.
func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if d.Name() == "lock" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
}

// The exit criterion: removal rotates the epoch, and the removed member
// cannot open the next one while the others can.
func TestRemovalRotatesAndTheRemovedCannotOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	for _, m := range []string{"alice", "bob", "carol"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   0 OPENED") {
			t.Fatalf("%s open:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
	r := h.must("org", "org", "remove", o.bob, "-group", o.group)
	if !strings.Contains(r.stdout, "epoch 1 (a new epoch") {
		t.Fatalf("remove:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, m := range []string{"alice", "carol"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   0 held") || !strings.Contains(r.stdout, "epoch   1 OPENED") {
			t.Fatalf("%s open after the removal:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
	r = h.run("bob", "", "open", o.group)
	h.want(r, exitRefused, "removed at seq 1")
	if !strings.Contains(r.stdout, "epoch   0 held") || !strings.Contains(r.stdout, "epoch   1 CANNOT OPEN") {
		t.Fatalf("bob open after the removal:\n%s\n%s", r.stdout, r.stderr)
	}
}

// Re-adding a removed key begins a new epoch, so the returning key
// reads from that epoch on and never the one written while it was away.
func TestRejoinRotates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	r := h.must("org", "org", "add", o.bob, "-group", o.group)
	if !strings.Contains(r.stdout, "epoch 2 (a new epoch") || !strings.Contains(r.stdout, "rejoin  "+o.bob) {
		t.Fatalf("rejoin:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   0 OPENED") || !strings.Contains(r.stdout, "epoch   1 CANNOT OPEN: not a member") ||
		!strings.Contains(r.stdout, "epoch   2 OPENED") {
		t.Fatalf("bob after the rejoin:\n%s\n%s", r.stdout, r.stderr)
	}
	// An addition of a key never removed keeps the epoch.
	dave := h.identity("dave", false)
	r = h.must("org", "org", "add", dave, "-group", o.group)
	if !strings.Contains(r.stdout, "grant   ") || !strings.Contains(r.stdout, "epoch 2\n") {
		t.Fatalf("add dave:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("dave", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   2 OPENED") || !strings.Contains(r.stdout, "epoch   0 CANNOT OPEN") {
		t.Fatalf("dave:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A member's key change removes the old key, so the epoch rotates.
func TestKeyChangeRotates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	alice2 := h.identity("alice2", false)
	r := h.must("org", "org", "rekey", o.alice, alice2)
	if !strings.Contains(r.stdout, "revokes 1 of the old key's") || !strings.Contains(r.stdout, "sweep   ") ||
		!strings.Contains(r.stdout, "epoch 1 (a new epoch") {
		t.Fatalf("rekey:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice2", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   1 OPENED") {
		t.Fatalf("the new key:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("alice", "", "open", o.group), exitRefused, "removed at seq 1")
	h.want(h.run("alice", "", "member", o.alice), exitIncomplete, "no host answers a grant")
	h.must("alice2", "member", alice2)
}

// A grant revoked by sweep is revoked for every verifier and retracted at
// every host; the roll still holds it until a token revokes it, and the
// member's group membership is rotation debt the next transition clears.
func TestRevokeBySweepAndDebt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	r := h.must("org", "grant", "revoke", o.carol, "-by", "sweep")
	if !strings.Contains(r.stdout, "sweep   ") || !strings.Contains(r.stderr, "not erasure") {
		t.Fatalf("revoke by sweep:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.run("carol", "", "member", o.carol)
	h.want(r, exitRefused, "revoked: its revocation outpoint is spent by")
	r = h.run("alice", "", "roll")
	if r.code != 0 || !strings.Contains(r.stdout, "not current, revoked") {
		t.Fatalf("roll:\n%s\n%s", r.stdout, r.stderr)
	}
	// The organisation's own record still has carol current until the
	// roll revokes her; revoking by the roll makes her debt.
	r = h.must("org", "grant", "revoke", o.carol, "-by", "roll")
	if !strings.Contains(r.stdout, "revokes 1 grant(s) in the roll") || !strings.Contains(r.stderr, "rotation debt") {
		t.Fatalf("revoke by roll:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "doctor")
	if !strings.Contains(r.stdout, "ROTATION DEBT: "+o.carol) {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	r = h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stdout, "removed "+o.carol+": rotation debt") || !strings.Contains(r.stdout, "epoch 1 (a new epoch") {
		t.Fatalf("rotate:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("carol", "", "open", o.group), exitRefused, "removed at seq 1")
	h.must("alice", "open", o.group)
}

// Offboarding revokes every grant by the roll and by sweep and removes the
// key from every group, each with its own new epoch.
func TestOffboard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	r := h.must("org", "group", "create", "ops", "-member", o.bob, "-member", o.carol)
	ops := groupLine.FindStringSubmatch(r.stdout)[1]
	r = h.must("org", "org", "remove", o.bob)
	if strings.Count(r.stdout, "(a new epoch") != 2 || !strings.Contains(r.stdout, "offboarded 1 key(s)") {
		t.Fatalf("offboard:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("bob", "", "open", o.group), exitRefused, "removed at seq 1")
	h.want(h.run("bob", "", "open", ops), exitRefused, "removed at seq 1")
	h.must("carol", "open", ops)
	r = h.run("bob", "", "member-of", o.bob)
	if r.code != 0 || !strings.Contains(r.stderr, "is a member of 0 group(s)") {
		t.Fatalf("member-of:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.run("carol", "", "member-of", o.carol)
	if r.code != 0 || !strings.Contains(r.stderr, "is a member of 2 group(s)") {
		t.Fatalf("member-of carol:\n%s\n%s", r.stdout, r.stderr)
	}
}

// In unicast every object goes to each host; a token must reach every host
// named, so with one down the token stays persisted (exit 2) and the next
// command publishes it before anything else.
func TestUnicastTokenNeedsEveryHostAndResumes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BORG_MODE"] = "unicast"
	delete(h.env, "BORG_FACADE")
	o := h.orgWithGroup()
	r := h.must("alice", "open", o.group)
	latest := regexpLatest.FindStringSubmatch(r.stdout)[1]
	if !h.a.Holds(latest) || !h.b.Holds(latest) {
		t.Fatal("unicast: the group token is not at both hosts")
	}
	h.b.SetDown(true)
	r = h.run("org", "", "epoch", "rotate", o.group)
	h.want(r, exitUsage, "1 of 2 host(s) took it and the quorum is 2")
	r = h.must("org", "doctor")
	if !strings.Contains(r.stdout, "outbox      group-token") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	h.b.SetDown(false)
	r = h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stderr, "publishing what a previous run persisted") || !strings.Contains(r.stdout, "epoch 2 (a new epoch") {
		t.Fatalf("resume:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, m := range []string{"alice", "bob", "carol"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   2 OPENED") {
			t.Fatalf("%s:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
}

// A host that lost a token disagrees with the other, and a reader says so
// rather than merge silently.
func TestHostsDisagree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	r := h.must("alice", "open", o.group)
	latest := regexpLatest.FindStringSubmatch(r.stdout)[1]
	h.b.Forget(latest)
	r = h.run("carol", "", "open", o.group)
	if r.code != 0 || !strings.Contains(r.stderr, "DISAGREES") || !strings.Contains(r.stdout, "epoch   1 OPENED") {
		t.Fatalf("open with a host behind:\n%s\n%s", r.stdout, r.stderr)
	}
	// A home that verified seq 1 refuses a host set that answers only seq 0.
	h.env["BORG_HOSTS"] = h.bS.URL
	h.want(h.run("alice", "", "open", o.group), exitIncomplete, "an older state")
}

// Two instances of the organisation's tooling build on one tip: one is
// mined, and the other loses, discards its epoch key and releases nothing
// (spec section 7.3).
func TestConcurrentTransitionLoses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	if err := copyDir(filepath.Join(h.dir, "org"), filepath.Join(h.dir, "org2")); err != nil {
		t.Fatal(err)
	}
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	r := h.run("org2", "", "org", "remove", o.carol, "-group", o.group)
	h.want(r, exitRefused, "lost: its predecessor is spent by")
	r = h.must("org2", "doctor")
	if strings.Contains(r.stdout, "transition  ") {
		t.Fatalf("the lost transition is still pending:\n%s", r.stdout)
	}
	h.must("carol", "open", o.group)
}

// The writer's check: positive evidence that the latest token is unspent.
func TestWriterEvidence(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	r := h.must("alice", "open", o.group, "-write")
	if !strings.Contains(r.stdout, "write   under epoch 0: the latest token") {
		t.Fatalf("open -write:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "open", o.group, "-write")
	if !strings.Contains(r.stdout, "write   under epoch 0: evidence") {
		t.Fatalf("open -write again:\n%s\n%s", r.stdout, r.stderr)
	}
	// Without a spend source there is no evidence, and a writer does not
	// write.
	delete(h.env, "BORG_ASSET")
	h.want(h.run("bob", "", "open", o.group, "-write"), exitIncomplete, "a writer does not write")
}

// The priced history question: the 402 is paid through AuthFetch, and the
// payee settles it.
func TestHistoryPaysThe402(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.must("bob", "fund", "-blocks", "102")
	h.startPaid(t, "payee", 7)
	r := h.must("bob", "terms")
	if !strings.Contains(r.stdout, "history        7 sat a question") {
		t.Fatalf("terms:\n%s", r.stdout)
	}
	r = h.must("bob", "history", "-chain", o.group)
	if !strings.Contains(r.stdout, "paid 7 sat") || !strings.Contains(r.stdout, "seq 0  ") {
		t.Fatalf("history:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("bob", "", "history", "-max-sats", "6"), exitUsage, "more than the 6 this command may pay")
	ledger := filepath.Join(h.dir, "payments.jsonl")
	if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.must("payee", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "1 payment(s) settled, 7 sat") || !h.chain.Mined(h.paid.Payments[0].Txid) {
		t.Fatalf("settle:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("payee", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "0 payment(s) settled, 0 sat; 1 settled before") {
		t.Fatalf("settle again:\n%s", r.stdout)
	}
}

// A host gives each session a budget of signed responses and refuses a
// request over it 429 without a signature. The command waits as the host
// says and asks once more, while it has paid nothing; refused twice, it
// says so. A refusal of the request that carried the payment is not asked
// again: that would take a second payment, and the first left this process,
// so it is kept as made and its coin is not reused.
func TestHistoryWaitsOutALimitedSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.must("bob", "fund", "-blocks", "102")
	h.startPaid(t, "payee", 7)
	// The question is refused: asked again, it is answered 402, paid and answered.
	h.paid.Limit(1)
	r := h.must("bob", "history", "-chain", o.group)
	if !strings.Contains(r.stdout, "paid 7 sat") || !strings.Contains(r.stderr, "waiting 1s and asking once more") || len(h.paid.Payments) != 1 {
		t.Fatalf("history, the question limited: %d payments\n%s\n%s", len(h.paid.Payments), r.stdout, r.stderr)
	}
	// The paid request is refused: the payment left this process, so it is
	// kept as made, broadcast by its payer, and not paid a second time.
	sent := h.chain.Sent
	h.paid.Limit(2)
	r = h.run("bob", "", "history", "-chain", o.group)
	h.want(r, exitIncomplete, "refused the paid request for its session's budget (429)")
	if !strings.Contains(r.stderr, "it is kept as made and its coins are not reused") || strings.Contains(r.stderr, "asking once more") ||
		len(h.paid.Payments) != 1 || h.chain.Sent != sent+1 {
		t.Fatalf("history, the payment limited: %d payments at the host, %d sent\n%s\n%s", len(h.paid.Payments), h.chain.Sent-sent, r.stdout, r.stderr)
	}
	// Refused twice: the command stops, and nothing was paid.
	h.paid.Limit(1, 2)
	h.want(h.run("bob", "", "history", "-chain", o.group), exitIncomplete, "(429) twice; ask again later")
	if len(h.paid.Payments) != 1 {
		t.Fatalf("payments %d", len(h.paid.Payments))
	}
	// And the wallet still pays, on a coin the payment it kept did not
	// spend.
	h.paid.Limit()
	r = h.must("bob", "history", "-chain", o.group)
	if !strings.Contains(r.stdout, "paid 7 sat") || len(h.paid.Payments) != 2 {
		t.Fatalf("history after: %d payments\n%s\n%s", len(h.paid.Payments), r.stdout, r.stderr)
	}
}

// A reader enumerates a group's members from the wraps of its current
// epoch: a member with no wrap is reported, and the group shown as
// inconsistent.
func TestGroupShowEnumeratesByWraps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	r := h.must("alice", "group", "show", "engineering")
	if strings.Count(r.stdout, "member  ") != 3 || !strings.Contains(r.stderr, "name exactly the 3 member(s)") {
		t.Fatalf("group show:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		for _, id := range host.Txids("wrap") {
			host.Forget(id)
		}
	}
	r = h.run("alice", "", "group", "show", o.group)
	h.want(r, exitIncomplete, "3 member(s) without a wrap")
	// Two blocks past the token, missing wraps are no longer pending.
	if _, err := h.chain.Generate(2, "1BgGZ9tcN4rm9KBzDn7KprQz87SZ26SAMH"); err != nil {
		t.Fatal(err)
	}
	r = h.run("alice", "", "group", "show", o.group)
	h.want(r, exitIncomplete, "is inconsistent: 0 stray wrap recipient(s), 3 member(s) without a wrap")
	h.want(h.run("dave", "", "open", o.group), exitUsage, "run `borg init`")
	h.want(h.run("carol", "", "open", o.group), exitIncomplete, "no host answers a wrap of epoch 0")
}

// With more than one tooling instance able to sign, wraps wait one block
// after their token (spec section 7.2), and the next command makes them.
func TestTwoSignersWrapAfterABlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BORG_SIGNERS"] = "2"
	o := h.orgWithGroup()
	h.want(h.run("alice", "", "open", o.group), exitIncomplete, "no host answers a wrap of epoch 0")
	// A reader sees the release pending, not inconsistent, for that block.
	h.want(h.run("alice", "", "group", "show", o.group), exitIncomplete, "PENDING")
	dave := h.identity("dave", false)
	r := h.must("org", "grant", "issue", dave)
	if !strings.Contains(r.stdout, "admits it") {
		t.Fatalf("grant issue:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   0 OPENED") {
		t.Fatalf("alice after a block:\n%s\n%s", r.stdout, r.stderr)
	}
}

// doctor reports a group whose epoch is older than its rotateEvery.
func TestRotationDue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "group", "create", "ops", "-member", o.alice, "-rotate-every", "1s")
	time.Sleep(2100 * time.Millisecond)
	r := h.must("org", "doctor")
	if !strings.Contains(r.stdout, "ROTATION DUE: epoch 0") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
}

// A name someone else wrote is filtered before it reaches a terminal.
func TestHostileNamesAreFiltered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	ops := h.namedGroup("ops\x1b]0;owned\x07\x1b[31m", o.alice)
	for _, args := range [][]string{{"group", "show", ops}, {"open", ops}, {"member-of", o.alice}, {"group", "list"}} {
		r := h.must("alice", args...)
		if strings.ContainsAny(r.stdout+r.stderr, "\x1b\x07") {
			t.Fatalf("%v:\n%q\n%q", args, r.stdout, r.stderr)
		}
	}
}

// A host that lost part of a chain, a grant and the current wraps is
// filled from the other host's answers: each token replayed in the BEEF a
// host admits (seq 0 with the chain tree read from the node), each carrier
// as the other host answered it; afterwards the hosts agree.
func TestFillReplaysWhatAHostLacks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	lost := append(h.b.Txids("token"), h.b.Txids("grant")[0])
	for _, id := range append(lost, h.b.Txids("wrap")...) {
		h.b.Forget(id)
	}
	r := h.run("alice", "", "group", "show", o.group)
	h.want(r, exitIncomplete, "DISAGREES")
	r = h.must("alice", "fill")
	if !strings.Contains(r.stdout, "organisation chain seq 0") || !strings.Contains(r.stdout, "group "+o.group+" seq 1") ||
		!strings.Contains(r.stdout, "filled  grant ") || !strings.Contains(r.stdout, "filled  wrap ") {
		t.Fatalf("fill:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, id := range lost {
		if !h.b.Holds(id) {
			t.Fatalf("host b still lacks %s after the fill:\n%s", id, r.stdout)
		}
	}
	// The current epoch's wraps are back: every member has one.
	r = h.must("alice", "group", "show", o.group)
	if strings.Contains(r.stderr, "DISAGREES") {
		t.Fatalf("after the fill:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "roll")
	if got := strings.Count(r.stderr, "reproduced"); got != 2 {
		t.Fatalf("roll after the fill:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "fill")
	if !strings.Contains(r.stdout, "filled  0 object(s)") {
		t.Fatalf("a second fill:\n%s", r.stdout)
	}
}

// On the plane a wrap is submitted once to the facade. A wrap the plane
// does not deliver to a host the publisher names is looked for there,
// found missing, and offered to that host directly; a fill replays the
// wraps of an ended epoch a host lacks too.
func TestPlaneWrapAHostMissedIsOfferedDirectly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lost := 0
	h.plane.Lost = func(host int, beef []byte) bool {
		_, tx, _, err := guard.ParseBEEF(beef, 8<<20)
		if err != nil || tx == nil || host != 1 || borgrec.Classify(*tx.Outputs[0].LockingScript) != borgrec.KindWrap {
			return false
		}
		lost++
		return true
	}
	o := h.orgWithGroup()
	if lost == 0 {
		t.Fatal("the plane lost no wrap on the way to host b")
	}
	wraps := h.a.Txids("wrap")
	if len(wraps) == 0 || len(h.b.Txids("wrap")) != len(wraps) {
		t.Fatalf("host a holds %d wrap(s), host b %d: the wrap the plane lost was not offered to b directly", len(wraps), len(h.b.Txids("wrap")))
	}
	r := h.must("alice", "group", "show", o.group)
	if strings.Contains(r.stderr, "DISAGREES") || strings.Contains(r.stdout, "INCOMPLETE") {
		t.Fatalf("group show:\n%s\n%s", r.stdout, r.stderr)
	}
	// An ended epoch's wrap a host lost is filled as well.
	h.plane.Lost = nil
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	h.b.Forget(wraps[0])
	r = h.must("alice", "fill")
	if !strings.Contains(r.stdout, "filled  wrap ") || !h.b.Holds(wraps[0]) {
		t.Fatalf("fill of an ended epoch's wrap:\n%s\n%s", r.stdout, r.stderr)
	}
}

// sorted is a listing's lines in order: two homes hold the same grants in
// the order each came to hold them.
func sorted(s string) string {
	l := strings.Split(s, "\n")
	sort.Strings(l)
	return strings.Join(l, "\n")
}

// block mines one empty block.
func (h *harness) block() {
	h.t.Helper()
	if _, err := h.chain.Generate(1, "1BgGZ9tcN4rm9KBzDn7KprQz87SZ26SAMH"); err != nil {
		h.t.Fatal(err)
	}
}

// secondHome makes a home around the organisation's identity key, as on a
// second machine, funds it, and catches it up from the hosts.
func (h *harness) secondHome(name string) result {
	h.t.Helper()
	r := h.must(name, "init", "-from", filepath.Join(h.dir, "org", "identity.json"))
	if !strings.Contains(r.stdout, "around the identity in") {
		h.t.Fatalf("init -from:\n%s\n%s", r.stdout, r.stderr)
	}
	h.must(name, "fund", "-blocks", "102")
	return h.run(name, "", "org", "sync")
}

// Two homes of one organisation race the same transition again and again.
// Each round one is mined and the other loses; the loser catches up from
// the hosts by itself (the chain, the roll, and the winner's epoch key
// from the wrap to the signer key) and makes its change on the new tip.
// Both homes stay usable, and every member opens every epoch.
func TestTwoHomesRaceAndBothStayUsable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BORG_SIGNERS"] = "2"
	o := h.orgWithGroup()
	h.block()
	r := h.secondHome("org2")
	if r.code != 0 || !strings.Contains(r.stdout, "took 1 organisation token(s), 1 group token(s) (1 new group(s)), 4 grant(s), 0 policy record(s), 0 epoch key(s)") {
		// The first home has not released epoch 0 yet (signers 2: one
		// block after the token, by its next command).
		if r.code != exitIncomplete || !strings.Contains(r.stdout, "epoch 0, the CURRENT epoch") {
			t.Fatalf("org sync on the second home:\n%s\n%s", r.stdout, r.stderr)
		}
	}
	h.must("org", "org", "sync")
	r = h.must("org2", "org", "sync")
	if !strings.Contains(r.stdout, "1 epoch key(s)") {
		t.Fatalf("the second home did not open epoch 0 from the wrap to the signer key:\n%s\n%s", r.stdout, r.stderr)
	}
	groupToken := func(tx *transaction.Transaction) bool {
		tok, err := borgrec.ReadToken(tx)
		return err == nil && tok.Group != nil
	}
	homes := []string{"org", "org2"}
	for round := 0; round < 4; round++ {
		winner, loser := homes[round%2], homes[(round+1)%2]
		h.chain.SetHoldIf(groupToken)
		done := make(chan result, 1)
		go func() { done <- h.run(winner, "", "epoch", "rotate", o.group) }()
		for deadline := time.Now().Add(20 * time.Second); h.chain.Waiting() == 0; {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: %s sent no transition", round, winner)
			}
			time.Sleep(2 * time.Millisecond)
		}
		// The loser builds on the same tip while the winner's is unmined.
		lr := h.run(loser, "", "epoch", "rotate", o.group)
		h.want(lr, exitRefused, "lost: its predecessor is spent by")
		h.chain.SetHoldIf(nil)
		h.chain.Mine()
		if wr := <-done; wr.code != 0 {
			t.Fatalf("round %d: %s: exit %d\n%s\n%s", round, winner, wr.code, wr.stdout, wr.stderr)
		}
		h.block()
		// The loser runs again: it catches up by itself and rotates on
		// the winner's token.
		lr = h.must(loser, "epoch", "rotate", o.group)
		if !strings.Contains(lr.stderr, "sync    took 0 organisation token(s), 1 group token(s)") || !strings.Contains(lr.stdout, "(a new epoch") {
			t.Fatalf("round %d: %s did not catch up and rotate:\n%s\n%s", round, loser, lr.stdout, lr.stderr)
		}
		h.block()
	}
	// Each home releases what it sampled and opens what the other did.
	for range 2 {
		for _, home := range homes {
			h.run(home, "", "org", "sync")
			h.block()
		}
	}
	var shows []string
	for _, home := range homes {
		r := h.must(home, "org", "sync")
		if strings.Contains(r.stdout, "missing ") {
			t.Fatalf("%s still lacks an epoch key:\n%s\n%s", home, r.stdout, r.stderr)
		}
		s := h.must(home, "org", "show").stdout
		if strings.Contains(s, "KEY NOT HELD") || !strings.Contains(s, "seq 8 epoch 8, 4 member(s)") || !strings.Contains(s, "(the organisation's signer key)") {
			t.Fatalf("%s org show:\n%s", home, s)
		}
		shows = append(shows, s)
	}
	if sorted(shows[0]) != sorted(shows[1]) {
		t.Fatalf("the two homes disagree:\n%s\n---\n%s", shows[0], shows[1])
	}
	// Both go on: one removes a member, the other adds one.
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	h.block()
	dave := h.identity("dave", false)
	r = h.must("org2", "org", "add", dave, "-group", o.group)
	// The first home has not released epoch 9 yet (one block after its
	// token), so the second cannot wrap it to dave: its addition begins an
	// epoch it holds.
	if !strings.Contains(r.stderr, "sync    took") || !strings.Contains(r.stderr, "does not hold the key of epoch 9") ||
		!strings.Contains(r.stdout, "epoch 10 (a new epoch") {
		t.Fatalf("the second home after the first's removal:\n%s\n%s", r.stdout, r.stderr)
	}
	h.block()
	for range 2 {
		for _, home := range homes {
			h.run(home, "", "org", "sync")
			h.block()
		}
	}
	for _, home := range homes {
		if r := h.must(home, "org", "sync"); strings.Contains(r.stdout, "missing ") {
			t.Fatalf("%s lacks an epoch key at the end:\n%s\n%s", home, r.stdout, r.stderr)
		}
	}
	r = h.must("alice", "open", o.group)
	for e := 0; e <= 10; e++ {
		if !strings.Contains(r.stdout, fmt.Sprintf("epoch   %d OPENED", e)) {
			t.Fatalf("alice cannot open epoch %d:\n%s\n%s", e, r.stdout, r.stderr)
		}
	}
	h.want(h.run("bob", "", "open", o.group), exitRefused, "removed at seq 9")
	r = h.must("dave", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   10 OPENED") || !strings.Contains(r.stdout, "epoch   9 CANNOT OPEN: not a member") {
		t.Fatalf("dave:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "group", "show", o.group)
	if !strings.Contains(r.stderr, "name exactly the 4 member(s)") {
		t.Fatalf("group show:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A home copied from another holds the same coins and the same funding
// trees. After the original moved on, the copy catches up: the coins the
// original spent leave its pool, and it stops spending the trees the
// original spends, so no two carriers share a funding output.
func TestCopiedHomeCatchesUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BORG_SIGNERS"] = "2"
	o := h.orgWithGroup()
	h.block()
	h.must("org", "org", "sync")
	if err := copyDir(filepath.Join(h.dir, "org"), filepath.Join(h.dir, "org2")); err != nil {
		t.Fatal(err)
	}
	dave := h.identity("dave", false)
	h.must("org", "org", "add", dave, "-group", o.group)
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	h.block()
	h.must("org", "org", "sync")
	r := h.must("org2", "org", "remove", o.carol, "-group", o.group)
	if !strings.Contains(r.stderr, "sync    took 1 organisation token(s), 2 group token(s) (0 new group(s)), 1 grant(s), 0 policy record(s), 1 epoch key(s)") ||
		strings.Count(r.stderr, "is no longer spent from this home") != 2 || !strings.Contains(r.stderr, "removed (spent by another instance)") ||
		!strings.Contains(r.stdout, "epoch 2 (a new epoch") {
		t.Fatalf("the copy:\n%s\n%s", r.stdout, r.stderr)
	}
	h.block()
	h.must("org2", "org", "sync")
	for _, m := range []string{"alice", "dave"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   2 OPENED") {
			t.Fatalf("%s:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
	h.want(h.run("carol", "", "open", o.group), exitRefused, "removed at seq 3")
	// The original catches up in turn and goes on.
	r = h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stderr, "sync    took 0 organisation token(s), 1 group token(s)") || !strings.Contains(r.stdout, "epoch 3 (a new epoch") {
		t.Fatalf("the original:\n%s\n%s", r.stdout, r.stderr)
	}
}

// An organisation run from one machine grants its signer key, and a home
// made on a new machine around the same identity key rebuilds from the
// hosts: the chains, the roll (an opaque grant's fields opened through its
// keyring), and each epoch key the signer key was a member of. An epoch
// begun before the signer key joined is named as not recoverable.
func TestNewMachineRebuildsFromHosts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var o org
	o.key = h.identity("org", true)
	o.alice = h.identity("alice", false)
	o.bob = h.identity("bob", false)
	h.newHall("org")
	h.env["BORG_ORG"] = o.key
	h.must("org", "org", "create", "Example Organisation", "-opaque", "-member", o.alice, "-member", o.bob, "-role", "engineer")
	r := h.must("org", "group", "create", "engineering", "-member", o.alice, "-member", o.bob)
	o.group = groupLine.FindStringSubmatch(r.stdout)[1]
	h.must("org", "epoch", "rotate", o.group)
	r = h.must("org", "org", "signer")
	if !strings.Contains(r.stdout, "signer  ") || !strings.Contains(r.stdout, "seq 2 ") {
		t.Fatalf("org signer:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "org", "signer")
	if !strings.Contains(r.stdout, "a member already") {
		t.Fatalf("org signer again:\n%s\n%s", r.stdout, r.stderr)
	}
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	want := h.must("org", "org", "show").stdout
	r = h.secondHome("org2")
	if r.code != 0 || !strings.Contains(r.stdout, "took 2 organisation token(s), 4 group token(s) (1 new group(s)), 3 grant(s), 0 policy record(s), 2 epoch key(s)") ||
		!strings.Contains(r.stdout, "missing group "+o.group+" epoch 0: the signer key was not a member of this epoch") {
		t.Fatalf("org sync on a new machine:\n%s\n%s", r.stdout, r.stderr)
	}
	if got := h.must("org2", "org", "show").stdout; sorted(got) != sorted(want) {
		t.Fatalf("the rebuilt home differs:\n%s\n---\n%s", got, want)
	}
	if !strings.Contains(want, " engineer member until ") {
		t.Fatalf("the opaque grants' fields were not read back:\n%s", want)
	}
	r = h.must("org2", "org", "sync")
	if !strings.Contains(r.stdout, "took 0 organisation token(s), 0 group token(s) (0 new group(s)), 0 grant(s), 0 policy record(s), 0 epoch key(s)") {
		t.Fatalf("a second sync:\n%s", r.stdout)
	}
	// The rebuilt home runs the organisation: it adds a member, who is
	// wrapped the current epoch's key, which only a home that holds it can
	// do.
	carol := h.identity("carol", false)
	r = h.must("org2", "org", "add", carol, "-group", o.group)
	if !strings.Contains(r.stdout, "epoch 2\n") {
		t.Fatalf("add from the rebuilt home:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("carol", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   2 OPENED") {
		t.Fatalf("carol:\n%s\n%s", r.stdout, r.stderr)
	}
	// The rebuilt home holds the trees of what the hosts answered, so its
	// kill switch reaches the grants and wraps the first home funded.
	r = h.must("org2", "kill", "-confirm", "KILL", "-yes")
	if strings.Count(r.stdout, "swept   grant tree") < 2 || !strings.Contains(r.stdout, "swept   record tree") {
		t.Fatalf("kill from the rebuilt home:\n%s\n%s", r.stdout, r.stderr)
	}
	if rr := h.run("alice", "", "member", o.alice); rr.code == 0 {
		t.Fatalf("alice is still a current member after the rebuilt home's kill:\n%s\n%s", rr.stdout, rr.stderr)
	}
	// A home that has an identity is not made again.
	h.want(h.run("org2", "", "init", "-from", filepath.Join(h.dir, "org", "identity.json")), exitUsage, "has an identity already")
}

// A copied home holds the same coins. A transition whose fee coin the
// other home spent can never mine although nothing beat it to its
// predecessor: it is discarded, not sent again for ever, and after a
// catch-up the home pays with a coin that is its own to spend.
func TestTransitionWhoseFeeCoinIsSpentElsewhereIsDiscarded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	r := h.must("org", "group", "create", "ops", "-member", o.alice)
	ops := groupLine.FindStringSubmatch(r.stdout)[1]
	if err := copyDir(filepath.Join(h.dir, "org"), filepath.Join(h.dir, "org2")); err != nil {
		t.Fatal(err)
	}
	h.must("org", "epoch", "rotate", o.group)
	r = h.run("org2", "", "epoch", "rotate", ops)
	h.want(r, exitRefused, "lost: its fee coin is spent by")
	r = h.must("org2", "doctor")
	if strings.Contains(r.stdout, "transition  ") {
		t.Fatalf("the transition that cannot mine is still pending:\n%s", r.stdout)
	}
	// No signer key in this organisation: the other home's epoch key is
	// not at the hosts for this one, and the catch-up says so.
	r = h.run("org2", "", "org", "sync")
	h.want(r, exitIncomplete, "the key of the current epoch 1 is not open here")
	if !strings.Contains(r.stdout, "took 0 organisation token(s), 1 group token(s)") ||
		!strings.Contains(r.stdout, "epoch 1, the CURRENT epoch") || !strings.Contains(h.must("org2", "doctor").stdout, "KEY NOT HELD: epoch 1") {
		t.Fatalf("org sync:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org2", "epoch", "rotate", ops)
	if !strings.Contains(r.stdout, "epoch 1 (a new epoch") {
		t.Fatalf("rotate after the catch-up:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "open", ops)
	if !strings.Contains(r.stdout, "epoch   1 OPENED") {
		t.Fatalf("alice:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A payer with one coin asks several priced questions inside one block:
// each payment after the first spends the change of the one before, which
// it carries, and the payee settles them all. Past the bound on unmined
// payments in a row the payer waits for a block.
func TestOneCoinPaysSeveralQuestionsInOneBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.startPaid(t, "payee", 7)
	// Exactly one coin is mature, and no block is mined from here on.
	h.must("bob", "fund", "-blocks", "101")
	h.chain.SetHold(true)
	for i := range limits.MaxUnminedPayments {
		r := h.must("bob", "history", "-chain", o.group)
		if !strings.Contains(r.stdout, "paid 7 sat") {
			t.Fatalf("question %d:\n%s\n%s", i, r.stdout, r.stderr)
		}
	}
	if n := len(h.paid.Payments); n != limits.MaxUnminedPayments {
		t.Fatalf("the host recorded %d payment(s), want %d", n, limits.MaxUnminedPayments)
	}
	if r := h.must("bob", "doctor"); !strings.Contains(r.stdout, "unmined payment(s) of this home's is spendable by its next payment") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	// Each payment spends the one before: one coin, never spent twice.
	for i := 1; i < limits.MaxUnminedPayments; i++ {
		tx := h.chain.Tx(h.paid.Payments[i].Txid)
		if tx == nil || tx.Inputs[0].SourceTXID.String() != h.paid.Payments[i-1].Txid {
			t.Fatalf("payment %d does not spend the change of payment %d", i, i-1)
		}
	}
	// One more would carry more unmined payments than the bound: the
	// payer waits for a block, and nothing was paid.
	r := h.run("bob", "", "history", "-chain", o.group)
	h.want(r, exitUsage, "whose proofs have not arrived")
	if n := len(h.paid.Payments); n != limits.MaxUnminedPayments {
		t.Fatalf("a payment past the bound was made: %d", n)
	}
	h.chain.SetHold(false)
	h.chain.Mine()
	ledger := filepath.Join(h.dir, "payments.jsonl")
	if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.must("payee", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, fmt.Sprintf("%d payment(s) settled, %d sat", limits.MaxUnminedPayments, 7*limits.MaxUnminedPayments)) {
		t.Fatalf("settle:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "history", "-chain", o.group)
	if !strings.Contains(r.stdout, "paid 7 sat") || !strings.Contains(h.must("bob", "doctor").stdout, "the change of 1 unmined payment(s)") {
		t.Fatalf("after the block:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A payer whose settlement leg takes nothing still pays in a row, and the
// payee, which holds each payment with the unmined payment it spends,
// broadcasts those first: nothing is left for the payer to do.
func TestPayeeBroadcastsThePaymentsAPaymentSpends(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.startPaid(t, "payee", 7)
	h.must("bob", "fund", "-blocks", "101")
	settle := h.env["BORG_SETTLE"]
	h.env["BORG_SETTLE"] = "rpc:http://127.0.0.1:1/rpc"
	h.chain.SetHold(true)
	for i := range 3 {
		r := h.must("bob", "history", "-chain", o.group)
		if !strings.Contains(r.stdout, "paid 7 sat") || !strings.Contains(r.stderr, "not broadcast from here") {
			t.Fatalf("question %d:\n%s\n%s", i, r.stdout, r.stderr)
		}
	}
	if h.chain.Waiting() != 0 {
		t.Fatal("a payment reached the chain although the payer's leg is down")
	}
	h.chain.SetHold(false)
	h.env["BORG_SETTLE"] = settle
	ledger := filepath.Join(h.dir, "payments.jsonl")
	// The last payment alone: the two it rests on are in its BEEF.
	lines := strings.Split(strings.TrimSpace(string(h.paid.Ledger())), "\n")
	if err := os.WriteFile(ledger, []byte(lines[2]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.must("payee", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "1 payment(s) settled, 7 sat") {
		t.Fatalf("settle:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, p := range h.paid.Payments {
		if !h.chain.Mined(p.Txid) {
			t.Fatalf("payment %s is not mined", p.Txid)
		}
	}
}

// org due lists the members whose grant is about to stop being current,
// then, once it has, the removal owed; -apply makes it, with a new epoch.
func TestOrgDueListsAndApplies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	dave := h.identity("dave", false)
	h.must("org", "org", "add", dave, "-group", o.group, "-expiry", "20s")
	// The listing, at chosen times: before the expiry inside and outside
	// the window, and after it.
	st, err := state.Load(filepath.Join(h.dir, "org"), o.key)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := borgrec.ParseExpiry(st.Org.Grants[len(st.Org.Grants)-1].Expiry)
	if err != nil {
		t.Fatal(err)
	}
	before := exp.Add(-10 * time.Second)
	for _, c := range []struct {
		now    time.Time
		within time.Duration
		n      int
		due    bool
	}{{before, time.Hour, 1, false}, {before, time.Second, 0, false}, {exp, 0, 1, true}, {exp.Add(time.Hour), time.Hour, 1, true}} {
		items, err := dueOf(st.Org, st.Identity, c.now, c.within)
		if err != nil || len(items) != c.n || (c.n == 1 && (items[0].member != dave || items[0].due != c.due || items[0].group.ID != o.group)) {
			t.Fatalf("dueOf at %s within %s: %+v, %v", c.now, c.within, items, err)
		}
	}
	var out bytes.Buffer
	items, _ := dueOf(st.Org, st.Identity, before, time.Hour)
	if n := printDue(&out, items, before); n != 0 || !strings.Contains(out.String(), "soon    group "+o.group+" engineering: "+dave+": its grant expires "+exp.UTC().Format(borgrec.ExpiryLayout)+", in 10s") {
		t.Fatalf("the listing before the expiry: %d\n%s", n, out.String())
	}
	if r := h.run("org", "", "doctor"); !strings.Contains(r.stdout, "1 member(s) hold a grant that expires within") && !strings.Contains(r.stdout, "ROTATION DEBT: "+dave) {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	// The command, once the grant has expired.
	var r result
	for deadline := time.Now().Add(90 * time.Second); ; {
		if r = h.run("org", "", "org", "due"); r.code == exitIncomplete || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.want(r, exitIncomplete, "1 removal(s) due")
	if !strings.Contains(r.stdout, "due     group "+o.group+" engineering: "+dave+": its grant expired ") {
		t.Fatalf("org due after the expiry:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "org", "due", "-apply")
	if !strings.Contains(r.stdout, "removed "+dave+": rotation debt") || !strings.Contains(r.stdout, "epoch 1 (a new epoch") ||
		!strings.Contains(r.stdout, "applied 1 group(s)") {
		t.Fatalf("org due -apply:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "org", "due")
	if strings.Contains(r.stdout, "due     ") {
		t.Fatalf("org due after -apply:\n%s", r.stdout)
	}
	r = h.must("org", "org", "due", "-apply")
	if !strings.Contains(r.stdout, "applied 0 group(s)") {
		t.Fatalf("a second -apply:\n%s", r.stdout)
	}
	h.want(h.run("dave", "", "open", o.group), exitRefused, "removed at seq 2")
	r = h.must("alice", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   1 OPENED") {
		t.Fatalf("alice:\n%s\n%s", r.stdout, r.stderr)
	}
}

// The kill switch sweeps every grant and record tree of the home: every
// grant is revoked for every verifier, and the hosts retract the grants
// and the wraps. Without -yes nothing is sent.
func TestKillSweepsEveryTree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("alice", "open", o.group)
	h.want(h.run("org", "", "kill"), exitUsage, "pass -confirm KILL")
	r := h.must("org", "kill", "-confirm", "KILL")
	if !strings.Contains(r.stdout, "would sweep grant tree") || !strings.Contains(r.stdout, "would sweep record tree") ||
		!strings.Contains(r.stdout, "nothing was sent") || !strings.Contains(r.stdout, "3 grant(s) of this home's record on it") {
		t.Fatalf("kill without -yes:\n%s\n%s", r.stdout, r.stderr)
	}
	if len(h.a.Txids("grant")) != 3 || len(h.a.Txids("sweep")) != 0 {
		t.Fatal("a dry run changed the hosts")
	}
	r = h.must("org", "kill", "-confirm", "KILL", "-yes")
	if !strings.Contains(r.stdout, "swept   grant tree") || !strings.Contains(r.stdout, "swept   record tree") ||
		!strings.Contains(r.stdout, "killed  ") || !strings.Contains(r.stderr, "not erasure") {
		t.Fatalf("kill:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, m := range []string{"alice", "bob", "carol"} {
		key := map[string]string{"alice": o.alice, "bob": o.bob, "carol": o.carol}[m]
		rr := h.run(m, "", "member", key)
		if rr.code == 0 || !(strings.Contains(rr.stdout+rr.stderr, "revoked") || strings.Contains(rr.stdout+rr.stderr, "no host answers a grant")) {
			t.Fatalf("%s is still a current member after the kill:\n%s\n%s", m, rr.stdout, rr.stderr)
		}
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		for _, id := range append(host.Txids("grant"), host.Txids("wrap")...) {
			if st := host.Status(id); st != borgrec.StatusRetracted {
				t.Fatalf("carrier %s is %s at a host after the kill", id, st)
			}
		}
	}
	// A member who had not opened the key finds no wrap; one who had
	// keeps what it opened.
	h.want(h.run("carol", "", "open", o.group), exitIncomplete, "no host answers a wrap of epoch 0")
	r = h.must("alice", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   0 held") {
		t.Fatalf("alice after the kill:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "kill", "-confirm", "KILL")
	if !strings.Contains(r.stdout, "0 tree(s) to sweep") {
		t.Fatalf("kill again:\n%s", r.stdout)
	}
	r = h.must("org", "doctor")
	if strings.Contains(r.stdout, "sweep       ") {
		t.Fatalf("doctor after the kill:\n%s", r.stdout)
	}
}

// watch is a command's standard error, which tells when a line with mark in
// it has been written.
type watch struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	mark string
	seen chan struct{}
	once sync.Once
}

func (w *watch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.mark) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *watch) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A funding tree minted ahead takes a coin when the mint starts, and its
// change reaches the pool only when the mint is collected. A home with one
// coin has none in between. The grant that leaves the grant tree low starts
// the mint, and the token that admits the grant needs a fee in that time:
// the take waits for the mint, collects it, and the token is paid from the
// tree's change once it mines, so the grant is issued and admitted in one
// command. When the settlement leg refuses the tree, the coin itself comes
// back at the collection and pays the token. Either way the token records
// its fee coin.
func TestATokenFeeWaitsForATreeMintedAhead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		refused bool
		notes   []string
	}{
		{"minted", false, []string{"is minted ahead and waits for the switch", "fee: every coin is change from 1 transaction(s) not yet mined; waiting for one"}},
		{"refused", true, []string{"the next funding tree was not minted ahead"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.orgWithGroup()
			dave := h.identity("dave", false)
			// The home is left with exactly one coin, as one that has
			// spent the others is.
			g, _, _ := h.global("org")
			hm, err := g.openHome()
			if err != nil {
				t.Fatal(err)
			}
			kept := false
			for _, o := range hm.e.Pool.Outputs() {
				if !kept && !o.Unproven && o.Spendable(h.chain.Height()) {
					kept = true
					continue
				}
				if _, err := hm.e.Pool.Remove(o); err != nil {
					t.Fatal(err)
				}
			}
			n := hm.e.Pool.Count()
			hm.close()
			if !kept || n != 1 {
				t.Fatalf("the home holds %d coins, one spendable %v, want 1", n, kept)
			}
			// Five grant outputs are left of eight: the grant leaves four,
			// so the next grant tree is minted ahead from the one coin.
			// That tree is the next transaction to reach the node. It is
			// kept from the node until the token's fee has been asked for,
			// so the mint is in flight then. The transaction after it is
			// the token, whose record names its fee coin by then.
			errw := &watch{mark: "no coin is spendable", seen: make(chan struct{})}
			var tree atomic.Value
			var recorded atomic.Bool
			h.mu.Lock()
			h.onSend = func(tx *transaction.Transaction) {
				tree.Store(tx.TxID().String())
				select {
				case <-errw.seen:
				case <-time.After(20 * time.Second):
				}
				time.Sleep(100 * time.Millisecond)
				h.mu.Lock()
				h.onSend = func(*transaction.Transaction) {
					var st struct {
						Org struct {
							Pending *struct {
								Fee *json.RawMessage `json:"fee"`
							} `json:"pending"`
						} `json:"org"`
					}
					if raw, err := os.ReadFile(filepath.Join(h.dir, "org", state.File)); err == nil && json.Unmarshal(raw, &st) == nil {
						recorded.Store(st.Org.Pending != nil && st.Org.Pending.Fee != nil)
					}
				}
				h.mu.Unlock()
			}
			if c.refused {
				h.refuse = func(tx *transaction.Transaction) bool { return tx.TxID().String() == tree.Load() }
			}
			h.mu.Unlock()
			var out bytes.Buffer
			env := func(k string) string {
				if k == "BORG_HOME" {
					return filepath.Join(h.dir, "org")
				}
				return h.env[k]
			}
			code := run(context.Background(), []string{"grant", "issue", dave}, strings.NewReader(""), &out, errw, env)
			if code != 0 || !strings.Contains(out.String(), "admits it") {
				t.Fatalf("grant issue: exit %d\n%s\n%s", code, out.String(), errw)
			}
			for _, want := range append([]string{"so the next is minted ahead", "1 funding tree(s) minted ahead hold one: waiting for the change"}, c.notes...) {
				if !strings.Contains(errw.String(), want) {
					t.Fatalf("no %q:\n%s", want, errw)
				}
			}
			if id, _ := tree.Load().(string); id == "" || h.chain.Mined(id) == c.refused {
				t.Fatalf("the tree minted ahead: mined %v, refused %v", !c.refused, c.refused)
			}
			if !recorded.Load() {
				t.Fatalf("the token did not record its fee coin:\n%s", errw)
			}
			if r := h.must("org", "doctor"); strings.Contains(r.stdout, "pending") {
				t.Fatalf("the home owes something:\n%s", r.stdout)
			}
		})
	}
}

// Roles decide: a role is defined, given to a group and to one key, and
// what each subject may do follows from the chain alone. Retiring a record
// takes it away, and a binding can never widen the role it names.
func TestRolesDecide(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()

	r := h.must("org", "role", "define", "releaser",
		"-permission", "bsecret/release/payroll", "-permission", "bstore/get/*")
	if !strings.Contains(r.stdout, "defines it") {
		t.Fatalf("role define:\n%s\n%s", r.stdout, r.stderr)
	}
	// The group holds the role, so every current member of it does.
	h.must("org", "bind", "releaser", "-to", o.group)
	// Alice also holds a narrower binding of her own, which cannot widen it.
	h.must("org", "bind", "releaser", "-to", o.alice, "-only", "bstore/get/*")

	for _, c := range []struct {
		key, permission, want string
	}{
		{o.alice, "bsecret/release/payroll", "ALLOW"},
		{o.alice, "bstore/get/photos/a.jpg", "ALLOW"},
		{o.alice, "bstore/put/photos/a.jpg", "DENY"},
		{o.bob, "bsecret/release/payroll", "ALLOW"},
	} {
		r := h.must("org", "policy", "check", c.key, c.permission)
		if !strings.HasPrefix(r.stdout, c.want) {
			t.Fatalf("check %s %s:\n%s", c.key[:8], c.permission, r.stdout)
		}
	}

	// A subject of no organisation holds nothing, whatever the set says.
	dave := h.identity("dave", false)
	r = h.must("org", "policy", "check", dave, "bsecret/release/payroll")
	if !strings.HasPrefix(r.stdout, "DENY") || !strings.Contains(r.stdout, "why     not-member") {
		t.Fatalf("a stranger was allowed:\n%s", r.stdout)
	}

	// The decision is read from the hosts and names the state it rested
	// on, which is what a service writes into a receipt. A member of the
	// organisation who is not its owner reaches the same verdict.
	r = h.must("alice", "policy", "check", o.alice, "bsecret/release/payroll")
	if !strings.HasPrefix(r.stdout, "ALLOW") {
		t.Fatalf("a member's own read of the hosts:\n%s\n%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "1 grant(s), 3 policy record(s), 1 group(s)") {
		t.Fatalf("the decision does not name what it rested on:\n%s", r.stdout)
	}
	// The same question answered from the home's own record, which only
	// the organisation holds.
	r = h.must("org", "policy", "check", o.alice, "bsecret/release/payroll", "-local")
	if !strings.HasPrefix(r.stdout, "ALLOW") {
		t.Fatalf("the local read:\n%s\n%s", r.stdout, r.stderr)
	}
	// The whole set is answered without a path: the host's answer is held
	// to the root instead, which show says once per host.
	r = h.must("alice", "policy", "show")
	if !strings.Contains(r.stdout, "the root of seq") || strings.Count(r.stdout, "\n") != 4 {
		t.Fatalf("policy show from the hosts:\n%s\n%s", r.stdout, r.stderr)
	}
	// One role's records are answered with a path into the policy root.
	r = h.must("alice", "policy", "show", "-role", "releaser")
	if strings.Count(r.stdout, "path INTO THE POLICY ROOT verified") != 3 {
		t.Fatalf("policy show -role:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "policy", "show", "-bound", o.alice)
	if strings.Count(r.stdout, "path INTO THE POLICY ROOT verified") != 1 {
		t.Fatalf("policy show -bound:\n%s\n%s", r.stdout, r.stderr)
	}

	// Retiring the definition takes the permissions away from everyone,
	// without touching a grant or a group.
	h.must("org", "role", "retire", "releaser")
	r = h.must("org", "policy", "check", o.alice, "bsecret/release/payroll")
	if !strings.HasPrefix(r.stdout, "DENY") {
		t.Fatalf("a retired role still authorised:\n%s", r.stdout)
	}
	r = h.must("org", "policy", "show")
	if strings.Contains(r.stdout, "role    ") {
		t.Fatalf("the retired definition is still in the set:\n%s", r.stdout)
	}
	// The bindings that named it stay in the set and authorise nothing.
	if strings.Count(r.stdout, "bind    ") != 2 {
		t.Fatalf("the bindings left the set:\n%s", r.stdout)
	}
	r = h.must("org", "role", "list")
	if strings.Contains(r.stdout, "releaser") {
		t.Fatalf("role list still names it:\n%s", r.stdout)
	}
}

// A permission a service asks must carry no wildcard, and a binding needs a
// subject the grammar accepts.
func TestPolicyRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "role", "define", "reader", "-permission", "s/get/a")
	for _, c := range []struct{ args []string }{
		{[]string{"policy", "check", o.alice, "s/get/*"}},
		{[]string{"policy", "check", o.alice, "nonsense"}},
		{[]string{"bind", "reader", "-to", "00"}},
		{[]string{"bind", "reader", "-to", o.alice, "-expiry", "2030-01-02 03:04:05"}},
		{[]string{"role", "define", "Reader", "-permission", "s/get/a"}},
		{[]string{"role", "define", "reader2", "-permission", "*/get/a"}},
	} {
		r := h.run("org", "", c.args...)
		if r.code == 0 {
			t.Fatalf("%v was accepted:\n%s", c.args, r.stdout)
		}
	}
}

// A second home of the organisation rebuilds the policy set from the hosts
// alone, and decides the same way the first does.
func TestSyncRebuildsThePolicySet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "role", "define", "releaser", "-permission", "bsecret/release/payroll")
	h.must("org", "role", "define", "auditor", "-permission", "bstore/get/audit/*")
	h.must("org", "bind", "releaser", "-to", o.group)
	h.must("org", "bind", "auditor", "-to", o.alice, "-expiry", "2099-12-31T23:59:59Z")
	// One record retired before the rebuild: the set the hosts answer is
	// what the latest token commits to, not everything ever published.
	h.must("org", "role", "retire", "auditor")
	// The signer key joins every group, so a second home can open the
	// current epoch and its sync is clean (spec section 7.4).
	h.must("org", "org", "signer")

	r := h.secondHome("org2")
	if r.code != 0 || !strings.Contains(r.stdout, "policy record(s)") {
		t.Fatalf("org sync on a second home:\n%s\n%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "3 policy record(s)") {
		t.Fatalf("the set the hosts answer is the one the latest token commits to:\n%s", r.stdout)
	}
	// The rebuilt home holds the same set, read from its own record.
	local := h.must("org2", "policy", "show", "-local").stdout
	if strings.Count(local, "\n") != 3 || !strings.Contains(local, "releaser") || strings.Contains(local, "auditor bstore") {
		t.Fatalf("the rebuilt home's set:\n%s", local)
	}
	// And decides the same way the first home does, from the hosts.
	for _, home := range []string{"org", "org2"} {
		got := h.must(home, "policy", "check", o.alice, "bsecret/release/payroll").stdout
		if !strings.HasPrefix(got, "ALLOW") {
			t.Fatalf("%s decides differently:\n%s", home, got)
		}
	}
}
