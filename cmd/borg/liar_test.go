package main

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/lightwebinc/borg/borgrec"
)

// lie answers a class with one byte of every output's BEEF changed: a host
// that substitutes bytes for what it holds. The answer no longer verifies
// as what was asked, and its subject (an Atomic BEEF names it in its
// header) still names the transaction.
func lie(classes ...string) func(q *borgrec.Question, outs []testOutput) []testOutput {
	return func(q *borgrec.Question, outs []testOutput) []testOutput {
		hit := false
		for _, c := range classes {
			hit = hit || q.Class.Name == c
		}
		if !hit {
			return outs
		}
		lied := make([]testOutput, len(outs))
		for i, o := range outs {
			lied[i] = o
			lied[i].Beef = append([]byte(nil), o.Beef...)
			lied[i].Beef[len(o.Beef)*6/10] ^= 1
		}
		return lied
	}
}

// A hall host that substitutes bytes in a wrap it answers is named, and
// what it answered is not used: beside an honest host the key opens from
// that one and the command is incomplete (exit 3), never refused; alone,
// nothing opens, the host is named, and a member is told the wrap is
// absent (exit 3), not that its key cannot open it. Every host is asked,
// so an honest host asked first does not hide a lying one asked after it.
func TestAHostSubstitutingAWrapIsNamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.b.Rewrite = lie("wrap", "wraps")
	r := h.run("alice", "", "open", o.group)
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "epoch   0 OPENED from "+h.aS.URL) ||
		!strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED wrap") || strings.Contains(r.stderr, "host "+h.aS.URL+": REFUSED") {
		t.Fatalf("open, one hall host substituting a wrap: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// The reader package says the same to a program: the refusal travels
	// with what opened.
	g, _, _ := h.global("alice")
	rd, err := g.reader()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	org, _ := hex.DecodeString(o.key)
	gv, err := rd.Group(ctx, h.hall, org, o.group)
	if err != nil {
		t.Fatal(err)
	}
	ag, _, _ := h.global("alice")
	e, err := ag.openWallet()
	if err != nil {
		t.Fatal(err)
	}
	op := rd.Open(ctx, e.Signer(), e.Signer().Identity.Compressed(), gv, 0)
	if op.Label != "" || op.Host != h.aS.URL || len(op.Refused) != 1 || op.Refused[0].Host != h.bS.URL {
		t.Fatalf("reader.Open, one host substituting: label %q host %q refused %+v", op.Label, op.Host, op.Refused)
	}
	// group show reads the wraps of the current epoch from every host.
	r = h.run("carol", "", "group", "show", o.group)
	if r.code != exitIncomplete || !strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED wrap") || strings.Count(r.stdout, "member  ") != 3 ||
		strings.Contains(r.stdout, "INCOMPLETE") {
		t.Fatalf("group show, one host substituting a wrap: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// Both hosts lie: nothing opens, both are named, and the wrap is absent.
	h.a.Rewrite = lie("wrap", "wraps")
	r = h.run("bob", "", "open", o.group)
	if r.code != exitIncomplete || strings.Contains(r.stdout, "OPENED") || !strings.Contains(r.stderr, "host "+h.aS.URL+": REFUSED wrap") ||
		!strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED wrap") || !strings.Contains(r.stderr, "no host answers a wrap of epoch 0 to this key that verifies") {
		t.Fatalf("open, every hall host substituting a wrap: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	op = rd.Open(ctx, e.Signer(), e.Signer().Identity.Compressed(), gv, 0)
	if op.Label != "entry" || len(op.Refused) != 2 {
		t.Fatalf("reader.Open, every host substituting: label %q refused %+v", op.Label, op.Refused)
	}
	h.a.Rewrite, h.b.Rewrite = nil, nil
	h.must("bob", "open", o.group)
}

// A hall host that answers, for the wrap of one epoch, the wrap of another
// (which verifies as a carrier) answered something other than what was
// asked: beside an honest host it is named and the command is incomplete;
// alone, a member of the epoch is told its wrap is absent (exit 3) and the
// host is named, never that the epoch does not open (exit 1).
func TestAHostAnsweringAnotherEpochsWrapIsNamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "epoch", "rotate", o.group)
	var mu sync.Mutex
	old := map[string][]testOutput{}
	keep := func(q *borgrec.Question, outs []testOutput) []testOutput {
		if q.Class.Name != "wrap" {
			return outs
		}
		mu.Lock()
		defer mu.Unlock()
		k := q.Members["member"]
		if q.Members["epoch"] == "0" {
			old[k] = outs
			return outs
		}
		if prev, ok := old[k]; ok {
			return prev
		}
		return outs
	}
	// Bob reads epoch 0 first (the rewrite remembers its wrap), then the
	// current epoch from an honest host and a host answering epoch 0's.
	h.b.Rewrite = keep
	r := h.run("bob", "", "open", o.group)
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "epoch   1 OPENED from "+h.aS.URL) ||
		!strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED wrap") {
		t.Fatalf("open, a host answering another epoch's wrap: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// Alice holds no key yet; both hosts answer epoch 0's wrap for epoch 1.
	h.a.Rewrite = keep
	r = h.run("alice", "", "open", o.group)
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "epoch   0 OPENED") || strings.Contains(r.stdout, "epoch   1 OPENED") ||
		!strings.Contains(r.stderr, "host "+h.aS.URL+": REFUSED wrap") || !strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED wrap") {
		t.Fatalf("open, every host answering another epoch's wrap: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// A host that substitutes bytes in a token or a grant it answers is named,
// and what it answered is not used: the chain, the roll and a membership
// are read from what verified, and the command is incomplete (exit 3),
// never refused: one host at fault does not make a check fail.
func TestAHostSubstitutingATokenOrAGrantIsNamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.b.Rewrite = lie("group", "group-seq")
	r := h.run("alice", "", "open", o.group)
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "epoch   0 OPENED") || !strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED token") {
		t.Fatalf("open, one host substituting a token: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	h.b.Rewrite = lie("roll", "member")
	r = h.run("carol", "", "roll")
	if r.code != exitIncomplete || strings.Count(r.stdout, ": CURRENT") != 3 || !strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED grant") {
		t.Fatalf("roll, one host substituting a grant: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = h.run("bob", "", "member", o.bob)
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "CURRENT member") || !strings.Contains(r.stderr, "host "+h.bS.URL+": REFUSED grant") {
		t.Fatalf("member, one host substituting a grant: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// The organisation's own catch-up names it too, and is incomplete.
	h.b.Rewrite = lie("roll", "wraps", "wrap")
	r = h.run("org", "", "org", "sync")
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "sync    host "+h.bS.URL+": REFUSED") {
		t.Fatalf("org sync, one host substituting: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	h.b.Rewrite = nil
	h.must("org", "org", "sync")
}
