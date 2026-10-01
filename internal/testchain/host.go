package testchain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/lightwebinc/borg/internal/borgrec"
)

// Host is a stand-in overlay host carrying borg halls: /submit runs the
// engine's SPV and the borg topic manager (borgrec.Admit) over the topic's
// previous coins, and /lookup answers the ls_borg classes from an index
// that folds each chain from seq 0 and keeps one outpoint row per funding
// outpoint (spec sections 10.2, 11.1 and 11.2). A refusal is recorded
// nowhere, so the same transaction offered again is decided afresh. The
// priced classes are refused on /lookup when Priced is set, as a host that
// sells them refuses them there. Retention, restore and the evidence caps
// are the real module's and are not modelled.
type Host struct {
	mu       sync.Mutex
	chain    *Chain
	halls    map[string]bool
	applied  map[string]bool
	tokens   map[string]*tokRow
	carriers map[string]*carRow
	sweeps   map[string]*sweepRow
	// coins are the outputs the host holds, by outpoint, with the topic.
	coins map[transaction.Outpoint]string
	// Down answers every request with 503.
	Down bool
	// Priced refuses the history classes on /lookup.
	Priced bool
	// Drop, when set, is asked about each submission; true discards it
	// unseen, as a plane that lost the object would.
	Drop func(tx *transaction.Transaction) bool
	// Submits counts submissions; Refused counts refusals by reason.
	Submits int
	Refused map[string]int
}

type tokRow struct {
	txid   string
	hall   string
	org    string
	chain  string
	seq    uint64
	prev   string
	stored []byte
	tok    *borgrec.ChainToken
	height uint32
	pos    uint64
}

type carRow struct {
	txid  string
	hall  string
	org   string
	beef  []byte
	c     [32]byte
	op    string
	grant *borgrec.Grant
	wrap  *borgrec.WrapRecord
}

type sweepRow struct {
	txid  string
	hall  string
	beef  []byte
	spent []string
}

// NewHost is a host carrying halls whose engine checks proofs against
// chain.
func NewHost(chain *Chain, halls ...string) *Host {
	h := &Host{chain: chain, halls: map[string]bool{}, applied: map[string]bool{}, tokens: map[string]*tokRow{},
		carriers: map[string]*carRow{}, sweeps: map[string]*sweepRow{}, coins: map[transaction.Outpoint]string{}, Refused: map[string]int{}}
	for _, hl := range halls {
		h.halls[hl] = true
	}
	return h
}

// Carry adds a hall.
func (h *Host) Carry(hall string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.halls[hall] = true
}

// SetDown takes the host down or brings it back.
func (h *Host) SetDown(down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Down = down
}

// Holds reports whether the host holds an object with txid.
func (h *Host) Holds(txid string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tokens[txid] != nil || h.carriers[txid] != nil || h.sweeps[txid] != nil
}

// Forget drops an object, as a host that lost it would: an honest host
// does not, but a test makes one that did.
func (h *Host) Forget(txid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tokens, txid)
	delete(h.carriers, txid)
	delete(h.sweeps, txid)
	delete(h.applied, txid)
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	down := h.Down
	h.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/submit":
		h.submit(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/lookup":
		h.lookup(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *Host) submit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body")
		return
	}
	topic := r.Header.Get("x-topics")
	hall := strings.TrimPrefix(topic, borgrec.TopicPrefix)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Submits++
	if !h.halls[hall] {
		writeError(w, http.StatusBadRequest, "topic not carried")
		return
	}
	_, tx, txid, err := guard.ParseBEEF(body, 8<<20)
	if err != nil || tx == nil {
		writeError(w, http.StatusBadRequest, "not a BEEF")
		return
	}
	steak := func(admit []int, retain []int) {
		if admit == nil {
			admit = []int{}
		}
		if retain == nil {
			retain = []int{}
		}
		writeJSON(w, map[string]any{topic: map[string]any{"outputsToAdmit": admit, "coinsToRetain": retain}})
	}
	if h.Drop != nil && h.Drop(tx) {
		steak(nil, nil)
		return
	}
	if h.applied[txid.String()] {
		steak(nil, nil)
		return
	}
	ok, verr := spv.Verify(context.Background(), tx, h.chain, nil)
	if verr != nil || !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("SPV: %v", verr))
		return
	}
	var prev []int
	for i, in := range tx.Inputs {
		if h.coins[transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex}] == topic {
			prev = append(prev, i)
		}
	}
	a, err := borgrec.Admit(context.Background(), body, prev, borgrec.Host{Hall: hall, Headers: h.chain})
	if err != nil {
		// Raised, as the topic manager raises: the engine records nothing
		// and answers the submitter that nothing was admitted.
		h.Refused[borgrec.Reason(err)]++
		steak(nil, nil)
		return
	}
	id := txid.String()
	h.applied[id] = true
	for _, i := range a.Outputs {
		h.coins[transaction.Outpoint{Txid: *txid, Index: uint32(i)}] = topic //nolint:gosec // an output index
	}
	switch a.Kind {
	case borgrec.TxGrant, borgrec.TxWrap:
		c := a.Carrier
		h.carriers[id] = &carRow{txid: id, hall: hall, org: hex.EncodeToString(c.Owner), beef: body, c: c.Commitment,
			op: c.Funding.String(), grant: c.Grant, wrap: c.Wrap}
	case borgrec.TxOrgToken, borgrec.TxGroupToken:
		alone := cloneTx(tx)
		stored, err := alone.AtomicBEEF(false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		row := &tokRow{txid: id, hall: hall, stored: stored, tok: a.Token, height: tx.MerklePath.BlockHeight}
		for _, l := range tx.MerklePath.Path[0] {
			if l.Hash != nil && *l.Hash == *tx.TxID() {
				row.pos = l.Offset
			}
		}
		if t := a.Token; t.Org != nil {
			row.org, row.chain, row.seq, row.prev = hex.EncodeToString(t.Org.Org), "org", t.Org.Seq, chainhash.Hash(t.Org.Prev).String()
		} else {
			row.org, row.chain, row.seq, row.prev = hex.EncodeToString(t.Group.Org), hex.EncodeToString(t.Group.Group[:]), t.Group.Seq, chainhash.Hash(t.Group.Prev).String()
		}
		h.tokens[id] = row
	case borgrec.TxSweep:
		row := &sweepRow{txid: id, hall: hall, beef: body}
		for _, op := range a.Spent {
			row.spent = append(row.spent, op.String())
		}
		h.sweeps[id] = row
	}
	steak(a.Outputs, a.Retain)
}

func cloneTx(tx *transaction.Transaction) *transaction.Transaction {
	cp, err := transaction.NewTransactionFromBytes(tx.Bytes())
	if err != nil {
		panic(err)
	}
	cp.MerklePath = tx.MerklePath
	return cp
}

// status is a carrier's status on its outpoint row (spec section 11.2).
func (h *Host) status(c *carRow) string {
	for _, s := range h.sweeps {
		for _, x := range s.spent {
			if x == c.op {
				return borgrec.StatusRetracted
			}
		}
	}
	for _, o := range h.carriers {
		if o.op == c.op && bytes.Compare(o.c[:], c.c[:]) < 0 {
			return borgrec.StatusSuperseded
		}
	}
	return borgrec.StatusHeld
}

// Status is the status of a carrier the host holds, or "".
func (h *Host) Status(txid string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.carriers[txid]; c != nil {
		return h.status(c)
	}
	return ""
}

func (h *Host) lookup(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Service string          `json:"service"`
		Query   json.RawMessage `json:"query"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || q.Service != borgrec.LookupService {
		writeError(w, http.StatusBadRequest, "not an ls_borg question")
		return
	}
	qq, err := borgrec.ParseQuery(q.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.mu.Lock()
	priced := h.Priced
	h.mu.Unlock()
	if !qq.Class.Free && priced {
		writeError(w, http.StatusBadRequest, qq.Class.Name+" is priced; ask it at the host's terms route")
		return
	}
	outs, err := h.Answer(qq)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"type": "output-list", "outputs": outs})
}

// Output is one answered output.
type Output struct {
	Beef        []byte `json:"beef"`
	OutputIndex int    `json:"outputIndex"`
	Context     []byte `json:"context,omitempty"`
}

// chainRows are the tokens of one chain, by seq.
func (h *Host) chainRows(hall, org, chain string) map[uint64][]*tokRow {
	out := map[uint64][]*tokRow{}
	for _, t := range h.tokens {
		if t.hall == hall && t.org == org && t.chain == chain {
			out[t.seq] = append(out[t.seq], t)
		}
	}
	for _, rs := range out {
		sort.Slice(rs, func(i, j int) bool { return rs[i].txid < rs[j].txid })
	}
	return out
}

// fold is the chain from seq 0 along the spends the host holds: the
// first-mined seq 0, then at each seq the token whose prev is the last.
func (h *Host) fold(hall, org, chain string) []*tokRow {
	rows := h.chainRows(hall, org, chain)
	var out []*tokRow
	for s := uint64(0); ; s++ {
		var best *tokRow
		for _, t := range rows[s] {
			if s > 0 && t.prev != out[s-1].txid {
				continue
			}
			if best == nil || t.height < best.height || (t.height == best.height && t.pos < best.pos) {
				best = t
			}
		}
		if best == nil {
			return out
		}
		out = append(out, best)
	}
}

// roll is the roll of the highest token whose org fold holds, with its seq.
func (h *Host) roll(hall, org string) ([][32]byte, uint64, bool) {
	f := borgrec.NewOrgFold()
	var roll [][32]byte
	var seq uint64
	ok := false
	for _, t := range h.fold(hall, org, "org") {
		if f.Apply(t.tok.Org) != borgrec.StatusConsistent {
			break
		}
		roll, seq, ok = f.Roll(), t.seq, true
	}
	return roll, seq, ok
}

// members are a group's members after the highest token whose fold holds.
func (h *Host) members(hall, org, group string) [][]byte {
	f := borgrec.NewGroupFold()
	var members [][]byte
	for _, t := range h.fold(hall, org, group) {
		if st := f.Apply(t.tok.Group); st == borgrec.StatusMembersMismatch {
			break
		}
		members = f.Members()
	}
	return members
}

func latestOf(rows map[uint64][]*tokRow) []*tokRow {
	var top uint64
	found := false
	for s := range rows {
		if !found || s > top {
			top, found = s, true
		}
	}
	if !found {
		return nil
	}
	return rows[top]
}

func limit[T any](a []T, n int) []T {
	if len(a) > n {
		return a[:n]
	}
	return a
}

// Answer answers a validated question.
func (h *Host) Answer(q *borgrec.Question) ([]Output, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := q.Members
	hall := m["hall"]
	if !h.halls[hall] {
		return nil, errors.New("this host does not carry the hall")
	}
	n := q.Class.Limit
	toks := func(rs []*tokRow) []Output {
		out := []Output{}
		for _, t := range limit(rs, n) {
			out = append(out, Output{Beef: t.stored})
		}
		return out
	}
	cars := func(rs []*carRow) []Output {
		sort.Slice(rs, func(i, j int) bool { return rs[i].txid < rs[j].txid })
		out := []Output{}
		for _, c := range rs {
			if a := m["after"]; a != "" && c.txid <= a {
				continue
			}
			out = append(out, Output{Beef: c.beef})
		}
		return limit(out, n)
	}
	switch q.Class.Name {
	case "org":
		return toks(latestOf(h.chainRows(hall, m["org"], "org"))), nil
	case "org-seq", "group-seq":
		chain := "org"
		if g := m["group"]; g != "" {
			chain = g
		}
		s, _ := strconv.ParseUint(m["seq"], 10, 64)
		return toks(h.chainRows(hall, m["org"], chain)[s]), nil
	case "group":
		return toks(latestOf(h.chainRows(hall, m["org"], m["group"]))), nil
	case "roll", "roll-after", "member":
		org := m["org"]
		if q.Class.Name != "member" {
			org = m["roll"]
		}
		roll, seq, ok := h.roll(hall, org)
		if !ok {
			return []Output{}, nil
		}
		in := map[[32]byte]bool{}
		for _, c := range roll {
			in[c] = true
		}
		var rs []*carRow
		for _, c := range h.carriers {
			if c.grant == nil || c.hall != hall || c.org != org || !in[c.c] {
				continue
			}
			if q.Class.Name == "member" && hex.EncodeToString(c.grant.Subject) != m["member"] {
				continue
			}
			rs = append(rs, c)
		}
		out := cars(rs)
		if q.Class.Name == "member" {
			for i := range out {
				_, tx, _, _ := guard.ParseBEEF(out[i].Beef, 8<<20)
				ic, err := borgrec.ProveGrant(seq, roll, [32]byte(*tx.TxID()))
				if err == nil {
					out[i].Context, _ = ic.Encode()
				}
			}
		}
		return out, nil
	case "groups", "groups-after", "member-of", "member-of-after":
		org := m["groupsOf"]
		if org == "" {
			org = m["org"]
		}
		groups := map[string]bool{}
		for _, t := range h.tokens {
			if t.hall == hall && t.org == org && t.chain != "org" {
				groups[t.chain] = true
			}
		}
		ids := make([]string, 0, len(groups))
		for g := range groups {
			if a := m["after"]; a != "" && g <= a {
				continue
			}
			if k := m["memberOf"]; k != "" {
				found := false
				for _, mem := range h.members(hall, org, g) {
					if hex.EncodeToString(mem) == k {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			ids = append(ids, g)
		}
		sort.Strings(ids)
		out := []Output{}
		for _, g := range limit(ids, n) {
			if rs := latestOf(h.chainRows(hall, org, g)); len(rs) > 0 {
				out = append(out, Output{Beef: rs[0].stored})
			}
		}
		return out, nil
	case "wraps", "wraps-after", "wrap":
		e, _ := strconv.ParseUint(m["epoch"], 10, 32)
		var rs []*carRow
		for _, c := range h.carriers {
			if c.wrap == nil || c.hall != hall || c.org != m["org"] || hex.EncodeToString(c.wrap.Group[:]) != m["group"] || c.wrap.Epoch != e {
				continue
			}
			if h.status(c) != borgrec.StatusHeld {
				continue
			}
			if k := m["member"]; k != "" {
				found := false
				for _, r := range c.wrap.Recipients() {
					if hex.EncodeToString(r) == k {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			rs = append(rs, c)
		}
		return cars(rs), nil
	case "sweep":
		out := []Output{}
		for _, s := range h.sweeps {
			if s.hall != hall {
				continue
			}
			for _, x := range s.spent {
				if x == m["spent"] {
					out = append(out, Output{Beef: s.beef})
				}
			}
		}
		return limit(out, n), nil
	case "history", "history-after":
		var rs []*tokRow
		for _, t := range h.tokens {
			if t.hall != hall || t.org != m["history"] || t.chain != m["chain"] {
				continue
			}
			if a := m["after"]; a != "" {
				after, _ := strconv.ParseUint(a, 10, 64)
				if t.seq <= after {
					continue
				}
			}
			rs = append(rs, t)
		}
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].seq != rs[j].seq {
				return rs[i].seq < rs[j].seq
			}
			return rs[i].txid < rs[j].txid
		})
		return toks(rs), nil
	}
	return nil, fmt.Errorf("class %s is not answered here", q.Class.Name)
}

// Plane stands in for the multicast plane: a facade that delivers each
// submission to every host subscribed to the topic, answering with the
// first host's answer. Lookups go to the first host, the facade's own.
type Plane struct {
	Hosts []http.Handler
	// Lost, when set, is asked per host and submission; true loses the
	// object on the way to that host.
	Lost func(host int, tx []byte) bool
}

func (p *Plane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/submit" {
		p.Hosts[0].ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	var first *recorder
	for i, h := range p.Hosts {
		if p.Lost != nil && p.Lost(i, body) {
			continue
		}
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(rec, req)
		if first == nil {
			first = rec
		}
	}
	if first == nil {
		http.Error(w, "lost", http.StatusServiceUnavailable)
		return
	}
	for k, v := range first.header {
		w.Header()[k] = v
	}
	w.WriteHeader(first.code)
	_, _ = w.Write(first.body.Bytes())
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(c int)           { r.code = c }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// Txids are the txids of the objects the host holds of a kind: "grant",
// "wrap", "token" or "sweep", sorted.
func (h *Host) Txids(kind string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	switch kind {
	case "grant", "wrap":
		for id, c := range h.carriers {
			if (kind == "grant") == (c.grant != nil) {
				out = append(out, id)
			}
		}
	case "token":
		for id := range h.tokens {
			out = append(out, id)
		}
	case "sweep":
		for id := range h.sweeps {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
