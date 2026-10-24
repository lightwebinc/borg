package module

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/replay"

	"github.com/lightwebinc/borg/borgrec"
)

// Ordered replay (bcommon module.Replayer): how this host repairs a gap on
// a hall's topic from a sibling host, as spec section 11.6 says a host is
// given a chain ("by replay, from the publisher or from anyone who reads a
// sibling host ... A replayer assembles each token's BEEF itself: the token
// from the org-seq or group-seq answer at seq, and its parent from the
// answer at seq - 1; at seq 0 the parent is the chain tree, which no host
// keeps, so it comes from the publisher or from a chain source by its txid,
// with its proof") and section 12 says a host that missed something is
// filled, never by an unordered synchronization of unspent outputs. The
// questions are the free classes of section 10.2, asked of the peer's
// ls_borg; everything answered is admitted here through the host's
// ordinary path, so every rule of section 11.1 holds.
//
// A walk is seven phases, each finished before the next reads the index:
//
//  1. groups: for each organization the host knows in the hall (from the
//     tokens and carriers it holds), the peer's groups pages (groups,
//     groups-after); a group the host holds no token of is kept for the
//     group tokens phase;
//  2. org tokens: each known organization's chain, the peer's latest (org),
//     then each seq below it the host holds no token at (org-seq);
//  3. group tokens: the same for every group the host holds a token of or
//     phase 1 found (group, group-seq). Each token is answered in the BEEF
//     of the token and its parent with minimal paths, held to section 11.1
//     before it is answered, parents first; a first token takes its parent,
//     the chain tree (the outpoint its input 0 spends, as the
//     organization's tooling builds it), from the chain view
//     (BORG_ASSET_URL or BORG_CHAIN);
//  4. roll: for each known organization, the grant carriers of the peer's
//     roll (roll, roll-after), which a host keeps while the roll holds them;
//  5. policy: likewise the policy carriers of the peer's policy set
//     (policy, policy-after);
//  6. wraps: for each group the host holds a token of, the held wraps of
//     its current epoch, and of an earlier epoch whose ending token the host
//     first saw inside the retention floor less a day (wraps, wraps-after):
//     older wraps have left every host by their floor, and asking for them
//     every walk would ask for nothing, ever more often;
//  7. sweeps: the sweeps of each held carrier's funding outpoint the host
//     has no sweep of (sweep).
//
// What no free class leads to is not replayed: an organization the host
// knows nothing of, a grant that left the roll, a retired policy record, a
// superseded or retracted wrap, a sweep that spends nothing a held carrier
// spends, a token the peer holds unrooted, and a chain's first token once
// the chain view no longer serves its chain tree with a proof; the
// organization's own fill does.

// KindsReplayed are the kinds a walk answers objects as.
var KindsReplayed = []string{replay.KindToken, "grant", "policy", "wrap", "sweep"}

var _ bmodule.Replayer = (*Service)(nil)

// Replay implements bcommon module.Replayer for tm_borg_ topics.
func (s *Service) Replay(ctx context.Context, r *bmodule.ReplayRequest) (*bmodule.ReplayAnswer, error) {
	hall, ok := s.hallOf(r.Topic)
	if !ok {
		return nil, bmodule.ErrNoReplay
	}
	if !s.Restored() {
		return nil, errors.New("ls_borg: the index is not restored yet")
	}
	w := replay.New(r)
	return w.Run(ctx, s.replayGroups(hall), s.replayOrgTokens(hall), s.replayGroupTokens(hall), s.replayRoll(hall), s.replayPolicy(hall),
		s.replayWraps(hall), s.replaySweeps(hall))
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// knownOrgs are the organizations the host knows in hall: those it holds a
// token or a carrier of.
func (s *Service) knownOrgs(hall string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	orgs := map[string]bool{}
	for _, ch := range s.chains {
		if ch.hall == hall && len(ch.byTxid) > 0 {
			orgs[ch.org] = true
		}
	}
	for _, e := range s.carriers {
		if e.hall == hall {
			orgs[e.org] = true
		}
	}
	return sortedKeys(orgs)
}

// ---- phase 1: the peer's groups of the organizations the host knows

func (s *Service) replayGroups(hall string) replay.Phase {
	return replay.Phase{Name: "groups",
		Items: func(context.Context, *replay.Walk) ([]string, error) { return s.knownOrgs(hall), nil },
		Do: func(ctx context.Context, w *replay.Walk, org string, page *string) (bool, error) {
			for {
				q := map[string]string{"hall": hall, "groupsOf": org}
				if *page != "" {
					q["after"] = *page
				}
				outs, err := w.Ask(ctx, borgrec.LookupService, q)
				if err != nil {
					return false, err
				}
				last := ""
				for _, o := range outs {
					t, err := borgrec.ReadToken(o.Tx)
					if err != nil || t.Group == nil || t.Group.Hall != hall || hex.EncodeToString(t.Group.Org) != org {
						continue
					}
					g := hex.EncodeToString(t.Group.Group[:])
					if g > last {
						last = g
					}
					w.Find("group|" + org + "|" + g)
				}
				if last == "" || last <= *page {
					return true, nil
				}
				*page = last
			}
		}}
}

// ---- phases 2 and 3: tokens

// groupItems are the groups the host holds a token of in hall, and those
// the walk found, as org|group.
func (s *Service) groupItems(hall string, w *replay.Walk) []string {
	s.mu.Lock()
	keys := map[string]bool{}
	for _, ch := range s.chains {
		if ch.hall == hall && ch.group != "" && len(ch.byTxid) > 0 {
			keys[ch.org+"|"+ch.group] = true
		}
	}
	s.mu.Unlock()
	if w != nil {
		for _, k := range w.Found("group|") {
			keys[strings.TrimPrefix(k, "group|")] = true
		}
	}
	return sortedKeys(keys)
}

func (s *Service) replayOrgTokens(hall string) replay.Phase {
	return replay.Phase{Name: "org-tokens",
		Items: func(context.Context, *replay.Walk) ([]string, error) { return s.knownOrgs(hall), nil },
		Do: func(ctx context.Context, w *replay.Walk, org string, page *string) (bool, error) {
			return w.Tokens(ctx, s.chainOf(hall, org, ""), page)
		}}
}

func (s *Service) replayGroupTokens(hall string) replay.Phase {
	return replay.Phase{Name: "group-tokens",
		Items: func(_ context.Context, w *replay.Walk) ([]string, error) { return s.groupItems(hall, w), nil },
		Do: func(ctx context.Context, w *replay.Walk, item string, page *string) (bool, error) {
			org, group, _ := strings.Cut(item, "|")
			return w.Tokens(ctx, s.chainOf(hall, org, group), page)
		}}
}

// chainOf is one chain for the walk's Tokens: the organization's (group
// "") or one group's.
func (s *Service) chainOf(hall, org, group string) *replay.Chain {
	q := map[string]string{"hall": hall, "org": org}
	name := "hall " + hall + " org " + short(org)
	kind := borgrec.KindOrg
	if group != "" {
		q["group"] = group
		name += " group " + short(group)
		kind = borgrec.KindGroup
	}
	key := chainKey(hall, org, group)
	return &replay.Chain{
		Name:    name,
		LatestQ: func() (string, map[string]string) { return borgrec.LookupService, q },
		AtSeq: func(seq uint64) (string, map[string]string) {
			at := map[string]string{"seq": strconv.FormatUint(seq, 10)}
			for k, v := range q {
				at[k] = v
			}
			return borgrec.LookupService, at
		},
		Held:  func(seq uint64) []chainhash.Hash { return s.heldTokens(key, seq) },
		Local: s.localToken,
		Read: func(tx *transaction.Transaction) (*replay.Token, error) {
			return readChainToken(tx, kind, hall, org, group)
		},
		Trees: s.trees,
		Check: func(ctx context.Context, beef []byte) error {
			a, err := borgrec.Admit(ctx, beef, nil, borgrec.Host{Hall: hall, Headers: s.headers, MaxBEEF: s.maxBEEF, MaxSweepInputs: s.maxSweep})
			if err != nil {
				return fmt.Errorf("%s: %w", borgrec.Reason(err), err)
			}
			if a.Kind != borgrec.TxOrgToken && a.Kind != borgrec.TxGroupToken {
				return fmt.Errorf("it would be admitted as a %s", a.Kind)
			}
			return nil
		},
	}
}

// heldTokens are the txids of the tokens the host holds of a chain at seq.
func (s *Service) heldTokens(key string, seq uint64) []chainhash.Hash {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.chains[key]
	if ch == nil {
		return nil
	}
	var out []chainhash.Hash
	for _, t := range ch.bySeq[seq] {
		if h, err := chainhash.NewHashFromHex(t.txid); err == nil {
			out = append(out, *h)
		}
	}
	return out
}

// localToken is a token the host holds, as the engine stores it: alone,
// with its proof (spec section 11.6).
func (s *Service) localToken(ctx context.Context, txid chainhash.Hash) (*transaction.Transaction, error) {
	s.mu.Lock()
	storage := s.storage
	var vout uint32
	found := false
	for _, ch := range s.chains {
		if t := ch.byTxid[txid.String()]; t != nil {
			vout, found = t.vout, true
			break
		}
	}
	s.mu.Unlock()
	if !found || storage == nil {
		return nil, nil
	}
	o, err := storage.FindOutput(ctx, &transaction.Outpoint{Txid: txid, Index: vout}, "")
	if err != nil || o == nil || o.Beef == nil {
		return nil, err
	}
	return parseFor(o.Beef, txid)
}

// readChainToken reads a token of one chain from its transaction alone:
// its seq, the token it spends, and at seq 0 the chain tree its input 0
// spends, as the organization's tooling builds a first token (the
// assembled BEEF is then held to section 11.1, whose rule 8 checks that an
// input spends a funding output of that tree locked to the chain key).
func readChainToken(tx *transaction.Transaction, kind borgrec.Kind, hall, org, group string) (*replay.Token, error) {
	t, err := borgrec.ReadToken(tx)
	if err != nil {
		return nil, err
	}
	if t.Kind != kind {
		return nil, errors.New("ls_borg: a token of another kind")
	}
	var seq uint64
	var prev [32]byte
	switch kind {
	case borgrec.KindOrg:
		if t.Org.Hall != hall || hex.EncodeToString(t.Org.Org) != org {
			return nil, errors.New("ls_borg: a token of another organization")
		}
		seq, prev = t.Org.Seq, t.Org.Prev
	default:
		if t.Group.Hall != hall || hex.EncodeToString(t.Group.Org) != org || hex.EncodeToString(t.Group.Group[:]) != group {
			return nil, errors.New("ls_borg: a token of another group")
		}
		seq, prev = t.Group.Seq, t.Group.Prev
	}
	if seq > 0 {
		return &replay.Token{Seq: seq, Prev: chainhash.Hash(prev)}, nil
	}
	if len(tx.Inputs) == 0 || tx.Inputs[0].SourceTXID == nil {
		return nil, errors.New("ls_borg: a first token with no input")
	}
	return &replay.Token{Tree: transaction.Outpoint{Txid: *tx.Inputs[0].SourceTXID, Index: tx.Inputs[0].SourceTxOutIndex}}, nil
}

// ---- phases 4 and 5: the roll's grants and the policy set's records

// pages asks a paged class for every output, kind naming them: q the first
// page, and each next page after the last txid answered, until a page
// holds fewer than the class's 64 outputs (one output a carrier, so a
// short page is the last).
func pages(ctx context.Context, w *replay.Walk, kind string, q map[string]string, page *string) (bool, error) {
	for {
		ask := map[string]string{}
		for k, v := range q {
			ask[k] = v
		}
		if *page != "" {
			ask["after"] = *page
		}
		outs, err := w.Each(ctx, kind, borgrec.LookupService, ask, nil)
		if err != nil {
			return false, err
		}
		last := ""
		for _, o := range outs {
			if id := o.Txid.String(); id > last {
				last = id
			}
		}
		if last == "" || last <= *page || len(outs) < PageSize {
			return true, nil
		}
		*page = last
	}
}

// PageSize is the most outputs a page of the roll, policy and wraps
// classes holds (spec section 10.2).
const PageSize = 64

func (s *Service) replayRoll(hall string) replay.Phase {
	return replay.Phase{Name: "roll",
		Items: func(context.Context, *replay.Walk) ([]string, error) { return s.knownOrgs(hall), nil },
		Do: func(ctx context.Context, w *replay.Walk, org string, page *string) (bool, error) {
			return pages(ctx, w, "grant", map[string]string{"hall": hall, "roll": org}, page)
		}}
}

func (s *Service) replayPolicy(hall string) replay.Phase {
	return replay.Phase{Name: "policy",
		Items: func(context.Context, *replay.Walk) ([]string, error) { return s.knownOrgs(hall), nil },
		Do: func(ctx context.Context, w *replay.Walk, org string, page *string) (bool, error) {
			return pages(ctx, w, "policy", map[string]string{"hall": hall, "policy": org}, page)
		}}
}

// ---- phase 6: wraps

// wrapEpochs are, as org|group|epoch, the epochs of hall's groups whose
// wraps the host keeps: each group's current epoch, and an earlier one
// whose ending token (the first the host saw of a later epoch) it first saw
// inside the retention floor less a day.
func (s *Service) wrapEpochs(hall string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.unix() - (s.retention - int64((24*time.Hour)/time.Second))
	keys := map[string]bool{}
	for _, ch := range s.chains {
		if ch.hall != hall || ch.group == "" || len(ch.byTxid) == 0 {
			continue
		}
		var latest uint64
		ended := map[uint64]int64{}
		epochs := map[uint64]bool{}
		for _, t := range ch.byTxid {
			if t.grpRec.Epoch > latest {
				latest = t.grpRec.Epoch
			}
			epochs[t.grpRec.Epoch] = true
		}
		for e := range epochs {
			for _, t := range ch.byTxid {
				if t.grpRec.Epoch <= e {
					continue
				}
				seen, ok := s.tokenSeen[t.txid]
				if !ok {
					seen = s.unix()
				}
				if at, has := ended[e]; !has || seen < at {
					ended[e] = seen
				}
			}
		}
		for e := range epochs {
			if e == latest || ended[e] > cutoff {
				keys[ch.org+"|"+ch.group+"|"+strconv.FormatUint(e, 10)] = true
			}
		}
	}
	return sortedKeys(keys)
}

func (s *Service) replayWraps(hall string) replay.Phase {
	return replay.Phase{Name: "wraps",
		Items: func(context.Context, *replay.Walk) ([]string, error) { return s.wrapEpochs(hall), nil },
		Do: func(ctx context.Context, w *replay.Walk, item string, page *string) (bool, error) {
			p := strings.Split(item, "|")
			return pages(ctx, w, "wrap", map[string]string{"hall": hall, "org": p[0], "group": p[1], "epoch": p[2]}, page)
		}}
}

// ---- phase 7: sweeps

func (s *Service) replaySweeps(hall string) replay.Phase {
	return replay.Phase{Name: "sweeps",
		Items: func(context.Context, *replay.Walk) ([]string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			ops := map[string]bool{}
			for _, e := range s.carriers {
				if e.hall != hall {
					continue
				}
				if r := s.rows[e.op]; r != nil && len(r.sweeps) > 0 {
					continue
				}
				ops[e.op] = true
			}
			out := sortedKeys(ops)
			sort.Strings(out)
			return out, nil
		},
		Do: func(ctx context.Context, w *replay.Walk, op string, _ *string) (bool, error) {
			_, err := w.Each(ctx, "sweep", borgrec.LookupService, map[string]string{"hall": hall, "spent": op}, nil)
			return err == nil, err
		}}
}
