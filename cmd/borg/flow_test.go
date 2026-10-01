package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

// Re-adding a removed key begins a new epoch (Q7), so the returning key
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

// A member's key change removes the old key, so the epoch rotates (Q6).
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
	r := h.must("org", "group", "create", "ops\x1b]0;owned\x07\x1b[31m", "-member", o.alice)
	ops := groupLine.FindStringSubmatch(r.stdout)[1]
	for _, args := range [][]string{{"group", "show", ops}, {"open", ops}, {"member-of", o.alice}, {"group", "list"}} {
		r := h.must("alice", args...)
		if strings.ContainsAny(r.stdout+r.stderr, "\x1b\x07") {
			t.Fatalf("%v:\n%q\n%q", args, r.stdout, r.stderr)
		}
	}
}
