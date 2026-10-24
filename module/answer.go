package module

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/borg/borgrec"
)

// ---- questions (spec section 10.2)

// Formula is one output an answer names, with its context when the class
// gives one.
type Formula struct {
	Txid    string
	Vout    uint32
	Context []byte
}

// Classified is a question, validated, and whether this host prices it.
type Classified struct {
	Q      *borgrec.Question
	Priced bool
}

// Classify validates a question and names its class, as the terms route
// and the engine ask it. A question for another service, one no class has,
// or one naming a hall the host does not carry is an error.
func (s *Service) Classify(service string, query []byte) (*Classified, error) {
	if service != borgrec.LookupService {
		return nil, fmt.Errorf("ls_borg: this service is %s, not %s", borgrec.LookupService, service)
	}
	q, err := borgrec.ParseQuery(query)
	if err != nil {
		return nil, errors.New("ls_borg: refused: " + strings.TrimPrefix(err.Error(), "borgrec: "))
	}
	hall := q.Members["hall"]
	if !s.halls[hall] {
		return nil, fmt.Errorf("ls_borg: this host does not carry hall %s", hall)
	}
	return &Classified{Q: q, Priced: !q.Class.Free && s.priced[q.Class.Name]}, nil
}

// groupsOf are the group ids of an organization in a hall, ascending,
// strictly after after.
func (s *Service) groupsOf(hall, org, after string) []string {
	var out []string
	for g := range s.groupsByOrg[hall+"|"+org] {
		if after == "" || g > after {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// Answer answers a classified question. paid says the question was paid
// for (or asked where no payment is due); a class this host prices is
// refused otherwise.
func (s *Service) Answer(c *Classified, paid bool) ([]Formula, error) {
	if c.Priced && !paid {
		return nil, fmt.Errorf("ls_borg: %s is priced on this host; ask its terms route's /lookup over BRC-104", c.Q.Class.Name)
	}
	s.host.Metrics().Inc("borg_lookups_total", map[string]string{"class": c.Q.Class.Name}, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	m := c.Q.Members
	hall := m["hall"]
	limit := c.Q.Class.Limit
	after, hasAfter := m["after"]
	tok := func(t *tokenEntry) Formula { return Formula{Txid: t.txid, Vout: t.vout} }
	car := func(e *carrierEntry) Formula { return Formula{Txid: e.txid} }
	chain := func(org, group string) *chainIdx { return s.chains[chainKey(hall, org, group)] }
	atSeq := func(ch *chainIdx, seq uint64) []*tokenEntry {
		if ch == nil {
			return nil
		}
		return ch.bySeq[seq]
	}
	latest := func(ch *chainIdx) []*tokenEntry {
		if ch == nil || len(ch.bySeq) == 0 {
			return nil
		}
		var top uint64
		first := true
		for seq := range ch.bySeq {
			if first || seq > top {
				top, first = seq, false
			}
		}
		return atSeq(ch, top)
	}
	toks := func(ts []*tokenEntry, n int) []Formula {
		out := []Formula{}
		for _, t := range ts {
			if len(out) >= n {
				break
			}
			out = append(out, tok(t))
		}
		return out
	}
	byTxid := func(cs map[string]bool, keep func(e *carrierEntry) bool) []*carrierEntry {
		var out []*carrierEntry
		for c := range cs {
			if e := s.carriers[c]; e != nil && keep(e) {
				out = append(out, e)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].txid < out[j].txid })
		if len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	afterOK := func(e *carrierEntry) bool { return !hasAfter || e.txid > after }
	held := func(cs map[string]bool) []*carrierEntry {
		return byTxid(cs, func(e *carrierEntry) bool { return s.statusOf(e.c) == borgrec.StatusHeld && afterOK(e) })
	}
	carriers := func(es []*carrierEntry) []Formula {
		out := make([]Formula, 0, len(es))
		for _, e := range es {
			out = append(out, car(e))
		}
		return out
	}
	seqOf := func(v string) uint64 { n, _ := strconv.ParseUint(v, 10, 64); return n }
	switch c.Q.Class.Name {
	case "org":
		return toks(latest(chain(m["org"], "")), limit), nil
	case "org-seq":
		return toks(atSeq(chain(m["org"], ""), seqOf(m["seq"])), limit), nil
	case "group":
		return toks(latest(chain(m["org"], m["group"])), limit), nil
	case "group-seq":
		return toks(atSeq(chain(m["org"], m["group"]), seqOf(m["seq"])), limit), nil
	case "roll", "roll-after":
		a := s.answerRoll(hall, m["roll"])
		if a == nil {
			return []Formula{}, nil
		}
		return carriers(byTxid(s.grantsByOrg[hall+"|"+m["roll"]], func(e *carrierEntry) bool { return a.has[e.c] && afterOK(e) })), nil
	case "member":
		a := s.answerRoll(hall, m["org"])
		if a == nil {
			return []Formula{}, nil
		}
		out := []Formula{}
		for _, e := range byTxid(s.grantsBySubject[hall+"|"+m["org"]+"|"+m["member"]], func(e *carrierEntry) bool { return a.has[e.c] }) {
			ctx, err := s.inclusion(a, e.c, false)
			if err != nil {
				return nil, err
			}
			f := car(e)
			f.Context = ctx
			out = append(out, f)
		}
		return out, nil
	case "policy", "policy-after":
		a := s.answerPolicy(hall, m["policy"])
		if a == nil {
			return []Formula{}, nil
		}
		return carriers(byTxid(s.policiesByOrg[hall+"|"+m["policy"]], func(e *carrierEntry) bool { return a.has[e.c] && afterOK(e) })), nil
	case "role", "role-after", "bound", "bound-after":
		a := s.answerPolicy(hall, m["org"])
		if a == nil {
			return []Formula{}, nil
		}
		from := s.policiesByTo[hall+"|"+m["org"]+"|"+m["boundTo"]]
		if strings.HasPrefix(c.Q.Class.Name, "role") {
			from = s.policiesByRole[hall+"|"+m["org"]+"|"+m["role"]]
		}
		out := []Formula{}
		for _, e := range byTxid(from, func(e *carrierEntry) bool { return a.has[e.c] && afterOK(e) }) {
			ctx, err := s.inclusion(a, e.c, true)
			if err != nil {
				return nil, err
			}
			f := car(e)
			f.Context = ctx
			out = append(out, f)
		}
		return out, nil
	case "groups", "groups-after":
		out := []Formula{}
		for _, g := range s.groupsOf(hall, m["groupsOf"], after) {
			ts := latest(chain(m["groupsOf"], g))
			if len(out) > 0 && len(out)+len(ts) > limit {
				break
			}
			for _, t := range ts {
				out = append(out, tok(t))
			}
		}
		if len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	case "member-of", "member-of-after":
		out := []Formula{}
		key, err := hex.DecodeString(m["memberOf"])
		if err != nil {
			return nil, err
		}
		for _, g := range s.groupsOf(hall, m["org"], after) {
			ch := chain(m["org"], g)
			if ch == nil {
				continue
			}
			t := s.answerToken(ch)
			if t == nil || !s.foldAt(ch, t).(groupFold).f.Holds(key) {
				continue
			}
			out = append(out, tok(t))
			if len(out) >= limit {
				break
			}
		}
		return out, nil
	case "wraps", "wraps-after":
		return carriers(held(s.wrapsByEpoch[hall+"|"+m["org"]+"|"+m["group"]+"|"+m["epoch"]])), nil
	case "wrap":
		return carriers(held(s.wrapsByMember[hall+"|"+m["org"]+"|"+m["group"]+"|"+m["epoch"]+"|"+m["member"]])), nil
	case "sweep":
		var sw []*sweepEntry
		for txid := range s.sweepsByOp[hall+"|"+m["spent"]] {
			if e := s.sweeps[txid]; e != nil {
				sw = append(sw, e)
			}
		}
		sort.Slice(sw, func(i, j int) bool {
			a, b := sw[i], sw[j]
			if a.height != b.height {
				return a.height < b.height
			}
			if a.offset != b.offset {
				return a.offset < b.offset
			}
			return a.txid < b.txid
		})
		out := []Formula{}
		for _, e := range sw {
			if len(out) >= limit {
				break
			}
			out = append(out, Formula{Txid: e.txid})
		}
		return out, nil
	case "history", "history-after":
		group := m["chain"]
		if group == "org" {
			group = ""
		}
		ch := chain(m["history"], group)
		if ch == nil {
			return []Formula{}, nil
		}
		var seqs []uint64
		for seq := range ch.bySeq {
			if !hasAfter || seq > seqOf(after) {
				seqs = append(seqs, seq)
			}
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		out := []Formula{}
		for _, seq := range seqs {
			ts := atSeq(ch, seq)
			// A page never splits a seq, so the cursor loses nothing.
			if len(out) > 0 && len(out)+len(ts) > limit {
				break
			}
			for _, t := range ts {
				out = append(out, tok(t))
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("ls_borg: no answer for class %s", c.Q.Class.Name)
}

// inclusion is the context of a member, role or bound answer: the
// carrier's inclusion path in the roll (policy false) or the policy set of
// the token a's seq names (spec section 10.2).
func (s *Service) inclusion(a *answered, c string, policy bool) ([]byte, error) {
	b, err := hex.DecodeString(c)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("ls_borg: commitment %q", c)
	}
	var ic *borgrec.InclusionContext
	if policy {
		ic, err = borgrec.ProvePolicy(a.seq, a.set, [32]byte(b))
	} else {
		ic, err = borgrec.ProveGrant(a.seq, a.set, [32]byte(b))
	}
	if err != nil {
		return nil, err
	}
	return ic.Encode()
}

// Hydrate is an answer with each output's BEEF, as the engine hydrates one,
// read from the host's storage. A context rides along.
func (s *Service) Hydrate(ctx context.Context, formulas []Formula) ([]bmodule.HydratedOutput, error) {
	s.mu.Lock()
	storage := s.storage
	s.mu.Unlock()
	if storage == nil {
		return nil, errors.New("ls_borg: storage is not restored yet")
	}
	out := make([]bmodule.HydratedOutput, 0, len(formulas))
	for _, f := range formulas {
		h, err := chainhash.NewHashFromHex(f.Txid)
		if err != nil {
			continue
		}
		o, err := storage.FindOutput(ctx, &transaction.Outpoint{Txid: *h, Index: f.Vout}, "")
		if err != nil {
			return nil, err
		}
		if o == nil || o.Beef == nil {
			continue
		}
		out = append(out, bmodule.HydratedOutput{Beef: o.Beef, OutputIndex: f.Vout, Context: f.Context})
	}
	return out, nil
}

// Lookup implements module.LookupService: asked through the engine, a
// class the host prices is refused here. The answer is hydrated here and
// carries each output's context (bcommon module.HydratedOutput).
func (s *Service) Lookup(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	c, err := s.Classify(q.Service, q.Query)
	if err != nil {
		return nil, err
	}
	formulas, err := s.Answer(c, false)
	if err != nil {
		return nil, err
	}
	outs, err := s.Hydrate(ctx, formulas)
	if err != nil {
		return nil, err
	}
	ans := &lookup.LookupAnswer{Type: lookup.AnswerTypeOutputList, Outputs: make([]*lookup.OutputListItem, 0, len(outs)), Result: outs}
	for _, o := range outs {
		ans.Outputs = append(ans.Outputs, &lookup.OutputListItem{Beef: o.Beef, OutputIndex: o.OutputIndex})
	}
	return ans, nil
}

// byteArray writes bytes as a JSON array of numbers, as the TypeScript
// route writes a number[].
type byteArray []byte

func (b byteArray) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(int(c)))
	}
	sb.WriteByte(']')
	return []byte(sb.String()), nil
}

// OutputJSON is one output of the route's answer.
type OutputJSON struct {
	Beef        byteArray `json:"beef"`
	OutputIndex uint32    `json:"outputIndex"`
	Context     byteArray `json:"context,omitempty"`
}

// OutputList is the engine's lookup answer as JSON.
type OutputList struct {
	Type    string       `json:"type"`
	Outputs []OutputJSON `json:"outputs"`
}

// RouteAnswerer is ls_borg as the terms route asks its questions: a priced
// answer is hydrated as the engine hydrates one.
type RouteAnswerer struct{ LS *Service }

var _ termsroute.Answerer = RouteAnswerer{}

// Restored implements termsroute.Answerer.
func (r RouteAnswerer) Restored() bool { return r.LS.Restored() }

// Classify implements termsroute.Answerer.
func (r RouteAnswerer) Classify(_ context.Context, q termsroute.Question) (string, error) {
	c, err := r.LS.Classify(q.Service, q.Query)
	if err != nil {
		return "", err
	}
	return c.Q.Class.Name, nil
}

// Answer implements termsroute.Answerer.
func (r RouteAnswerer) Answer(ctx context.Context, q termsroute.Question) (any, error) {
	c, err := r.LS.Classify(q.Service, q.Query)
	if err != nil {
		return nil, err
	}
	formulas, err := r.LS.Answer(c, true)
	if err != nil {
		return nil, err
	}
	outs, err := r.LS.Hydrate(ctx, formulas)
	if err != nil {
		return nil, err
	}
	list := OutputList{Type: "output-list", Outputs: make([]OutputJSON, 0, len(outs))}
	for _, o := range outs {
		list.Outputs = append(list.Outputs, OutputJSON{Beef: o.Beef, OutputIndex: o.OutputIndex, Context: o.Context})
	}
	return list, nil
}
