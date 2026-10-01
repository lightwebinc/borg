package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndTheRatchet(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir, "02aa")
	if err != nil {
		t.Fatal(err)
	}
	s.AddHall("example_abcdefghij")
	s.AddHall("example_abcdefghij")
	s.Org = &Org{Hall: "example_abcdefghij", Name: "x", Groups: []Group{{ID: "g1", Epochs: []Epoch{{Epoch: 3, Key: "k"}}}}}
	s.AddKey(EpochKey{Hall: "h", Org: "o", Group: "g", Epoch: 1, Key: "k1"})
	s.AddKey(EpochKey{Hall: "h", Org: "o", Group: "g", Epoch: 1, Key: "k2"})
	s.See(Seen{Hall: "h", Org: "o", Chain: "g", Seq: 5, Txid: "a"})
	s.See(Seen{Hall: "h", Org: "o", Chain: "g", Seq: 4, Txid: "b"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, File))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	s2, err := Load(dir, "02aa")
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Halls) != 1 || len(s2.Keys) != 1 || s2.Keys[0].Key != "k1" || s2.SeenOf("h", "o", "g").Seq != 5 ||
		s2.Org.Group("g1").EpochOf(3).Key != "k" || s2.Org.Group("g2") != nil {
		t.Fatalf("%+v", s2)
	}
	if _, err := Load(dir, "02bb"); err == nil {
		t.Error("another identity's state loaded")
	}
}

func TestOutboxAndSweeps(t *testing.T) {
	s, _ := Load(t.TempDir(), "02aa")
	s.Outbox = []Pending{{Kind: KindGrant, Txid: "a"}, {Kind: KindOrgToken, Txid: "b"}}
	if !s.Queued("a") || s.Outbox[0].Mined() || !s.Outbox[1].Mined() {
		t.Fatal("outbox")
	}
	s.Done("a")
	if s.Queued("a") || len(s.Outbox) != 1 {
		t.Fatal("done")
	}
	s.Sweeps = []Sweep{{Txid: "x", Done: true}, {Txid: "y", Failed: "no"}, {Txid: "z"}}
	if s.InFlight().Txid != "z" {
		t.Fatal("in flight")
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second lock: %v", err)
	}
	unlock()
	unlock2, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}
