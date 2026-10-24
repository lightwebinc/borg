package module

import (
	"container/list"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/guard"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/replay"

	"github.com/lightwebinc/borg/borgrec"
)

// ls_borg: one lookup service for every hall a host carries (spec sections
// 10.2, 11.2 to 11.5), the twin of the TypeScript module's ls_borg.ts.
//
// Two stores back it. The engine's storage holds the outputs the host
// keeps: chain tokens, grant, policy and wrap carriers, and sweep
// tombstones, which this service indexes in memory and rebuilds from
// storage on start. The journal holds what the host keeps for its life
// beside them: every carrier any hall admitted with the funding outpoint
// it spent, every drop, every spend of such an outpoint by an admitted
// sweep, the first sight of every token, and every held sweep whose
// tombstone was spent. Spends of outpoints no carrier used live only in a
// bounded table, oldest dropped first, so a stranger's chain of mined
// sweeps costs the host that table and nothing more.
//
// Every status is a function of what the host holds, never of arrival
// order:
//
//   - a carrier is retracted when an admitted sweep spends its funding
//     outpoint, superseded when it is not its outpoint's winner (the lowest
//     C in hash byte order) or the winner was dropped, and held otherwise;
//   - a token is pending while a token on its path to seq 0 is not held,
//     and otherwise takes the status the fold of its chain gives it along
//     that path (borgrec.OrgFold, borgrec.GroupFold): consistent,
//     roll-mismatch, policy-mismatch, members-mismatch, commitment-reuse
//     or rejoin-unrotated.
//
// The roll, member, member-of, policy, role and bound answers use the fold
// of a chain's highest token that is not pending, the first mined among
// several.

// The index's bounds (spec section 11.2).
const (
	// EvidenceWraps and EvidenceGrants are the superseded carriers kept per
	// outpoint as evidence: wraps, and grants and policy records.
	EvidenceWraps  = 8
	EvidenceGrants = 64
	// MaxPerSeq is how many tokens are kept at one seq of one chain.
	MaxPerSeq = 8
)

// TokenStatuses are the token statuses the index reports (spec section
// 11.2), each counted.
var TokenStatuses = []string{borgrec.StatusConsistent, borgrec.StatusPending, borgrec.StatusRollMismatch, borgrec.StatusMembersMismatch,
	borgrec.StatusCommitmentReuse, borgrec.StatusRejoinUnrotated}

// Deleter is a storage that can delete an output: what retention and the
// outpoint rows need beyond the contract's RestoreStorage. A host whose
// storage cannot delete keeps what the index dropped, unanswered.
type Deleter interface {
	DeleteOutput(ctx context.Context, outpoint *transaction.Outpoint, topic string) error
}

// Options build a Service.
type Options struct {
	// Halls are the hall identifiers the host carries.
	Halls   []string
	Host    bmodule.Host
	Journal Journal
	// Priced are the priceable classes this host prices: asked through the
	// engine, they are refused.
	Priced map[string]bool
	// RetentionDays is how long a host keeps what it no longer answers, at
	// least FloorDays (31 when zero); SpendTable the bound on spends of
	// outpoints no carrier used (MinSpendTable when zero; the floor is held
	// where an operator sets it, so a test may name a smaller one).
	RetentionDays int
	SpendTable    int
	// Now is the host's clock; time.Now when nil.
	Now func() time.Time
	// Trees is the chain view a replay reads a chain's first token's parent
	// from (Replay); nil when the host has none.
	Trees replay.TreeSource
	// Headers are the host's block headers, and MaxBEEF and MaxSweepInputs
	// the topic managers' bounds, which a replay holds a token it
	// assembles to; borgrec's defaults when zero.
	Headers                 chaintracker.ChainTracker
	MaxBEEF, MaxSweepInputs int
}

// tokenEntry is a chain token the host holds.
type tokenEntry struct {
	txid  string
	vout  uint32
	kind  string // "org" or "group"
	hall  string
	org   string
	group string // the group id in hex; empty for an organization's chain
	chain string
	seq   uint64
	// prev is the predecessor's txid in display order; empty at seq 0.
	prev   string
	orgRec *borgrec.Org
	grpRec *borgrec.Group
	height uint32
	offset uint64
}

// chainFold is an organization's or a group's fold, for the chain it is
// of.
type chainFold interface {
	apply(t *tokenEntry) string
	clone() chainFold
}

type orgFold struct{ f *borgrec.OrgFold }

func (o orgFold) apply(t *tokenEntry) string { return o.f.Apply(t.orgRec) }
func (o orgFold) clone() chainFold           { return orgFold{o.f.Clone()} }

type groupFold struct{ f *borgrec.GroupFold }

func (g groupFold) apply(t *tokenEntry) string { return g.f.Apply(t.grpRec) }
func (g groupFold) clone() chainFold           { return groupFold{g.f.Clone()} }

type turnedToken struct {
	vout uint32
	seq  uint64
}

// chainIdx is one chain's tokens and its fold.
type chainIdx struct {
	kind  string
	hall  string
	org   string
	group string
	bySeq map[uint64][]*tokenEntry
	// counts is the status each token was last counted with. Forgetting
	// one token clears every status in the chain, and the refold then
	// decides them all again; without this they would be counted again too.
	counts map[string]string
	byTxid map[string]*tokenEntry
	// children are, by prev txid, the txids of tokens naming it.
	children map[string]map[string]bool
	status   map[string]string
	// states is the fold after a token, kept for tokens with no successor
	// yet.
	states map[string]chainFold
	// turned are tokens a full seq turned away while they were pending, by
	// the txid they name as their predecessor: txid -> output and seq. They
	// stay in the engine's storage, and are read again once that
	// predecessor is decided.
	turned map[string]map[string]turnedToken
}

func (ch *chainIdx) newFold() chainFold {
	if ch.kind == "org" {
		return orgFold{borgrec.NewOrgFold()}
	}
	return groupFold{borgrec.NewGroupFold()}
}

// carrierEntry is a grant, policy or wrap carrier the host holds.
type carrierEntry struct {
	// n orders carriers as the host first indexed them (the TypeScript
	// module's map order), which is the order retention visits them in.
	n    uint64
	kind string // "g", "p" or "w"
	c    string
	txid string
	hall string
	org  string
	op   string
	// subject is a grant's.
	subject string
	// group, epoch and recipients are a wrap's.
	group      string
	epoch      uint64
	recipients []string
	// role and to are a policy record's: its role, and what it binds to (a
	// group id or a key), empty for a definition.
	role string
	to   string
}

// sweepEntry is a sweep tombstone the host holds.
type sweepEntry struct {
	txid string
	// halls are every hall the sweep was admitted in: one mined sweep may
	// be published to several topics.
	halls  map[string]bool
	spent  []string
	height uint32
	offset uint64
	score  float64
}

type spend struct {
	hall string
	h    int64
	at   int64
}

// row is one funding outpoint's row.
type row struct {
	// carriers are the carriers the row keeps, by commitment: its winner
	// and the lowest superseded ones up to the kind's evidence bound. A
	// carrier past the bound is dropped and its line is not kept, so this
	// map is bounded and the row's memory does not grow with equivocation.
	carriers map[string]*Line
	// dropped are commitments dropped from the index, so that none is
	// admitted again.
	dropped map[string]bool
	// more counts the carriers the row stopped keeping a line for.
	more int
	// closed: its winner was dropped, and nothing on it is answered again.
	closed bool
	// sweeps are the admitted sweeps that spend it.
	sweeps map[string]spend
}

// tableEntry is one outpoint of the bounded table: its spends by sweep
// txid, in the order they arrived.
type tableEntry struct {
	op     string
	spends map[string]spend
	order  []string
}

// spendTable is the bounded table of spends of outpoints no carrier used,
// outpoints in the order they were first met, oldest first.
type spendTable struct {
	l *list.List
	m map[string]*list.Element
}

func newSpendTable() spendTable { return spendTable{l: list.New(), m: map[string]*list.Element{}} }

func (t *spendTable) get(op string) *tableEntry {
	if e := t.m[op]; e != nil {
		return e.Value.(*tableEntry)
	}
	return nil
}

func (t *spendTable) add(op string) *tableEntry {
	e := &tableEntry{op: op, spends: map[string]spend{}}
	t.m[op] = t.l.PushBack(e)
	return e
}

func (t *spendTable) remove(op string) *tableEntry {
	e := t.m[op]
	if e == nil {
		return nil
	}
	delete(t.m, op)
	return t.l.Remove(e).(*tableEntry)
}

func (t *spendTable) oldest() *tableEntry {
	if e := t.l.Front(); e != nil {
		return e.Value.(*tableEntry)
	}
	return nil
}

func (t *spendTable) size() int { return len(t.m) }

func (t *spendTable) has(op, txid string) bool {
	e := t.get(op)
	if e == nil {
		return false
	}
	_, ok := e.spends[txid]
	return ok
}

type pendingDelete struct {
	txid  string
	vout  uint32
	topic string
}

type rereadToken struct {
	hall string
	txid string
	vout uint32
}

// Service is ls_borg. Its methods are safe for concurrent use; an
// admission that meets a restore in flight waits for it (the restore holds
// the service's lock), so that it is decided as any other.
type Service struct {
	mu        sync.Mutex
	halls     map[string]bool
	host      bmodule.Host
	journal   Journal
	priced    map[string]bool
	retention int64
	tableCap  int
	now       func() time.Time
	trees     replay.TreeSource
	headers   chaintracker.ChainTracker
	maxBEEF   int
	maxSweep  int

	// What the journal holds.
	rows map[string]*row
	// lines are the lines of the carriers the rows keep, by commitment.
	lines map[string]*Line
	// droppedIn are dropped carriers, by commitment: the outpoint alone,
	// not the line.
	droppedIn map[string]string
	tokenSeen map[string]int64

	// What the host holds, from storage.
	chains          map[string]*chainIdx
	carriers        map[string]*carrierEntry
	carrierN        uint64
	grantsBySubject map[string]map[string]bool
	grantsByOrg     map[string]map[string]bool
	policiesByOrg   map[string]map[string]bool
	policiesByRole  map[string]map[string]bool
	policiesByTo    map[string]map[string]bool
	wrapsByEpoch    map[string]map[string]bool
	wrapsByMember   map[string]map[string]bool
	groupsByOrg     map[string]map[string]bool
	sweeps          map[string]*sweepEntry
	sweepsByOp      map[string]map[string]bool
	table           spendTable
	// spentSweeps are held sweeps whose tombstone an admitted transaction
	// spent, by txid, with the halls it happened in: storage no longer
	// hands them over among its unspent rows, so the journal names them.
	spentSweeps map[string]map[string]bool
	// reread are tokens to read again from storage: their predecessor is
	// decided now.
	reread []rereadToken
	// pending are outputs dropped from the index and still to be deleted
	// from storage.
	pending        map[string]pendingDelete
	storage        bmodule.RestoreStorage
	restoring      bool
	warnedNoDelete bool
	// carrierByTxid is a held carrier's txid -> its commitment.
	carrierByTxid map[string]string
}

var (
	_ bmodule.LookupService     = (*Service)(nil)
	_ bmodule.Restorer          = (*Service)(nil)
	_ bmodule.RetentionListener = (*Service)(nil)
)

// NewService makes ls_borg over its journal, which it loads.
func NewService(o Options) (*Service, error) {
	days := o.RetentionDays
	if days == 0 {
		days = FloorDays
	}
	if days < FloorDays {
		return nil, fmt.Errorf("ls_borg: retention of %d days is under the %d-day floor", days, FloorDays)
	}
	tableCap := o.SpendTable
	if tableCap == 0 {
		tableCap = MinSpendTable
	}
	if tableCap < 1 {
		return nil, fmt.Errorf("ls_borg: a spend table of %d", tableCap)
	}
	if o.Journal == nil {
		return nil, errors.New("ls_borg: no journal")
	}
	s := &Service{halls: map[string]bool{}, host: o.Host, journal: o.Journal, priced: o.Priced, retention: int64(days) * 86400, tableCap: tableCap,
		now: o.Now, trees: o.Trees, headers: o.Headers, maxBEEF: o.MaxBEEF, maxSweep: o.MaxSweepInputs}
	for _, h := range o.Halls {
		s.halls[h] = true
	}
	if s.priced == nil {
		s.priced = map[string]bool{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxBEEF == 0 {
		s.maxBEEF = borgrec.DefaultMaxBEEF
	}
	if s.maxSweep == 0 {
		s.maxSweep = borgrec.DefaultMaxSweepInputs
	}
	s.clear()
	lines, err := s.journal.Load()
	if err != nil {
		return nil, err
	}
	for i := range lines {
		s.apply(&lines[i])
	}
	return s, nil
}

// clear empties every map, as a restore starts.
func (s *Service) clear() {
	s.rows = map[string]*row{}
	s.lines = map[string]*Line{}
	s.droppedIn = map[string]string{}
	s.tokenSeen = map[string]int64{}
	s.chains = map[string]*chainIdx{}
	s.carriers = map[string]*carrierEntry{}
	s.grantsBySubject = map[string]map[string]bool{}
	s.grantsByOrg = map[string]map[string]bool{}
	s.policiesByOrg = map[string]map[string]bool{}
	s.policiesByRole = map[string]map[string]bool{}
	s.policiesByTo = map[string]map[string]bool{}
	s.wrapsByEpoch = map[string]map[string]bool{}
	s.wrapsByMember = map[string]map[string]bool{}
	s.groupsByOrg = map[string]map[string]bool{}
	s.sweeps = map[string]*sweepEntry{}
	s.sweepsByOp = map[string]map[string]bool{}
	s.table = newSpendTable()
	s.spentSweeps = map[string]map[string]bool{}
	s.reread = nil
	s.pending = map[string]pendingDelete{}
	s.carrierByTxid = map[string]string{}
}

func (s *Service) unix() int64 { return s.now().Unix() }

func addTo(m map[string]map[string]bool, k, v string) {
	set := m[k]
	if set == nil {
		set = map[string]bool{}
		m[k] = set
	}
	set[v] = true
}

func removeFrom(m map[string]map[string]bool, k, v string) {
	set := m[k]
	if set == nil {
		return
	}
	delete(set, v)
	if len(set) == 0 {
		delete(m, k)
	}
}

func outpoint(txid string, vout uint32) string {
	return txid + "." + strconv.FormatUint(uint64(vout), 10)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- counts

// Counts are what the gauges read.
type Counts struct {
	Tokens, PendingTokens, Grants, Policies, Wraps, Sweeps, Rows, Lines, Forgotten, Table, PendingDeletes int
}

// Counts reads every gauge at once.
func (s *Service) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Counts
	for _, ch := range s.chains {
		c.Tokens += len(ch.byTxid)
		for _, t := range ch.byTxid {
			if s.tokenStatusIn(ch, t) == borgrec.StatusPending {
				c.PendingTokens++
			}
		}
	}
	for _, e := range s.carriers {
		switch e.kind {
		case "g":
			c.Grants++
		case "p":
			c.Policies++
		default:
			c.Wraps++
		}
	}
	c.Sweeps = len(s.sweeps)
	c.Rows = len(s.rows)
	c.Lines = len(s.lines)
	for _, r := range s.rows {
		c.Forgotten += r.more
	}
	c.Table = s.table.size()
	c.PendingDeletes = len(s.pending)
	return c
}

// ---- the journal

func (s *Service) rowOf(op string) *row {
	r := s.rows[op]
	if r == nil {
		r = &row{carriers: map[string]*Line{}, dropped: map[string]bool{}, sweeps: map[string]spend{}}
		s.rows[op] = r
	}
	return r
}

func (s *Service) apply(l *Line) {
	switch l.K {
	case "c":
		c := *l
		s.rowOf(l.Op).carriers[l.C] = &c
		s.lines[l.C] = &c
	case "d":
		r := s.rowOf(l.Op)
		r.dropped[l.C] = true
		s.droppedIn[l.C] = l.Op
		// A dropped carrier is out of every answer, so its line is not
		// kept: the row remembers the commitment, so that it is never
		// admitted again, and counts the line it stopped keeping.
		if _, ok := r.carriers[l.C]; ok {
			delete(r.carriers, l.C)
			r.more++
		}
		delete(s.lines, l.C)
		if l.W != nil && *l.W {
			r.closed = true
		}
	case "s":
		r := s.rowOf(l.Op)
		if _, ok := r.sweeps[l.S]; !ok {
			var h int64
			if l.H != nil {
				h = *l.H
			}
			r.sweeps[l.S] = spend{hall: l.Hall, h: h, at: l.at()}
		}
	case "t":
		if _, ok := s.tokenSeen[l.Txid]; !ok {
			s.tokenSeen[l.Txid] = l.at()
		}
	case "w":
		addTo(s.spentSweeps, l.Txid, l.Hall)
	}
}

func (s *Service) queueDelete(txid string, vout uint32, hall string) {
	t := borgrec.Topic(hall)
	s.pending[t+"|"+outpoint(txid, vout)] = pendingDelete{txid: txid, vout: vout, topic: t}
}

// write appends a line to the journal and applies it. The time Append
// takes is spent under the service's lock (borg_journal_wait_seconds_total):
// the write, and the fsync too when the journal syncs every line. A line
// that is not on disk is still applied: the host keeps answering as it
// decided, and says so.
func (s *Service) write(l Line) {
	start := time.Now()
	err := s.journal.Append(l)
	m := s.host.Metrics()
	m.Inc("borg_journal_wait_seconds_total", nil, time.Since(start).Seconds())
	m.Inc("borg_journal_lines_total", nil, 1)
	if err != nil {
		s.host.Log("ls_borg could not write the journal", map[string]any{"err": err.Error()})
	}
	s.apply(&l)
}

// ---- carrier statuses (spec section 11.2)

// winnerOf is the outpoint's winner: the lowest C, hash byte order, over
// every carrier the row keeps.
func (s *Service) winnerOf(op string) string {
	r := s.rows[op]
	if r == nil {
		return ""
	}
	w := ""
	for c := range r.carriers {
		if w == "" || c < w {
			w = c
		}
	}
	return w
}

// statusOf is the status of the carrier with commitment c (hash byte order
// hex), "" when the host has no record of it.
func (s *Service) statusOf(c string) string {
	op := ""
	if l := s.lines[c]; l != nil {
		op = l.Op
	} else if o, ok := s.droppedIn[c]; ok {
		op = o
	}
	if op == "" {
		return ""
	}
	r := s.rows[op]
	if len(r.sweeps) > 0 {
		return borgrec.StatusRetracted
	}
	// A dropped carrier keeps no line, and is superseded whatever else
	// holds: the row no longer answers it.
	if r.closed || r.dropped[c] || s.winnerOf(op) != c {
		return borgrec.StatusSuperseded
	}
	return borgrec.StatusHeld
}

// Status is the status of the carrier with commitment c (hash byte order
// hex): retracted, superseded or held, and false when the host has no
// record of it.
func (s *Service) Status(c string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.statusOf(c)
	return st, st != ""
}

// Dropped reports whether the carrier with commitment c was dropped.
func (s *Service) Dropped(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.droppedIn[c]
	return ok
}

// ---- token statuses (spec section 11.2)

func chainKey(hall, org, group string) string {
	if group == "" {
		group = "org"
	}
	return hall + "|" + org + "|" + group
}

// TokenStatus is the status of the token with txid (display order), and
// false when the host does not hold it.
func (s *Service) TokenStatus(txid string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.chains {
		if t := ch.byTxid[txid]; t != nil {
			return s.tokenStatusIn(ch, t), true
		}
	}
	return "", false
}

// tokenStatusIn is a token's status along its path to seq 0: pending when
// a token on it is not held, otherwise what the fold gives it. A fold state
// is kept only for tokens no held token succeeds; a later fork is folded
// again from seq 0.
func (s *Service) tokenStatusIn(ch *chainIdx, t *tokenEntry) string {
	if memo, ok := ch.status[t.txid]; ok {
		return memo
	}
	var path []*tokenEntry
	var base chainFold
	var owner *tokenEntry
	cur := t
	for {
		if cur != t {
			if st := ch.states[cur.txid]; st != nil {
				base = st.clone()
				owner = cur
				break
			}
		}
		path = append(path, cur)
		if cur.seq == 0 {
			base = ch.newFold()
			break
		}
		p := ch.byTxid[cur.prev]
		if p == nil || p.seq != cur.seq-1 {
			for _, x := range path {
				ch.status[x.txid] = borgrec.StatusPending
			}
			return borgrec.StatusPending
		}
		cur = p
	}
	for i := len(path) - 1; i >= 0; i-- {
		x := path[i]
		st := base.apply(x)
		s.counted(ch, x, st)
		ch.status[x.txid] = st
	}
	ch.states[t.txid] = base
	if owner != nil {
		s.release(ch, owner)
	}
	return ch.status[t.txid]
}

// release lets go of a token's fold once every token naming it is decided.
func (s *Service) release(ch *chainIdx, t *tokenEntry) {
	for c := range ch.children[t.txid] {
		st, ok := ch.status[c]
		if !ok || st == borgrec.StatusPending {
			return
		}
	}
	delete(ch.states, t.txid)
}

func (s *Service) counted(ch *chainIdx, t *tokenEntry, st string) {
	if ch.counts[t.txid] == st {
		return
	}
	ch.counts[t.txid] = st
	s.host.Metrics().Inc("borg_token_status_total", map[string]string{"status": st}, 1)
	if st != borgrec.StatusConsistent {
		chain := ch.group
		if chain == "" {
			chain = "org"
		}
		s.host.Log("ls_borg token status", map[string]any{"hall": ch.hall, "org": ch.org, "chain": chain, "seq": t.seq, "txid": t.txid, "status": st})
	}
}

// sortedTokens are a chain's tokens by seq, then txid.
func sortedTokens(ch *chainIdx) []*tokenEntry {
	out := make([]*tokenEntry, 0, len(ch.byTxid))
	for _, t := range ch.byTxid {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].seq != out[j].seq {
			return out[i].seq < out[j].seq
		}
		return out[i].txid < out[j].txid
	})
	return out
}

// refold decides again every token of the chain whose status was pending.
// A token a full seq turned away while it was pending is queued to be read
// again once the predecessor it names is decided, when it is that
// predecessor's next seq; one at any other seq stays pending for good and
// is let go.
func (s *Service) refold(ch *chainIdx) {
	for txid, st := range ch.status {
		if st == borgrec.StatusPending {
			delete(ch.status, txid)
		}
	}
	for _, t := range sortedTokens(ch) {
		s.tokenStatusIn(ch, t)
	}
	prevs := make([]string, 0, len(ch.turned))
	for prev := range ch.turned {
		prevs = append(prevs, prev)
	}
	sort.Strings(prevs)
	for _, prev := range prevs {
		p := ch.byTxid[prev]
		if p == nil || ch.status[prev] == borgrec.StatusPending {
			continue
		}
		m := ch.turned[prev]
		delete(ch.turned, prev)
		txids := make([]string, 0, len(m))
		for txid := range m {
			txids = append(txids, txid)
		}
		sort.Strings(txids)
		for _, txid := range txids {
			if t := m[txid]; t.seq == p.seq+1 {
				s.reread = append(s.reread, rereadToken{hall: ch.hall, txid: txid, vout: t.vout})
			}
		}
	}
}

// bound holds a seq to its bound: tokens that are not pending before
// pending ones, then the lowest txids. So what is kept is a function of
// what the host holds, and a token on a predecessor no host has cannot take
// the place of one on the chain. A pending token turned away is remembered
// by its predecessor.
func (s *Service) bound(ch *chainIdx, seq uint64) {
	at := ch.bySeq[seq]
	if len(at) <= MaxPerSeq {
		return
	}
	pending := map[string]bool{}
	for _, t := range at {
		pending[t.txid] = s.tokenStatusIn(ch, t) == borgrec.StatusPending
	}
	order := append([]*tokenEntry(nil), at...)
	sort.SliceStable(order, func(i, j int) bool {
		pi, pj := pending[order[i].txid], pending[order[j].txid]
		if pi != pj {
			return !pi
		}
		return order[i].txid < order[j].txid
	})
	for _, t := range order[MaxPerSeq:] {
		s.host.Log("ls_borg kept no more tokens at this seq", map[string]any{"hall": ch.hall, "txid": t.txid, "seq": t.seq})
		if pending[t.txid] && t.prev != "" {
			m := ch.turned[t.prev]
			if m == nil {
				m = map[string]turnedToken{}
				ch.turned[t.prev] = m
			}
			m[t.txid] = turnedToken{vout: t.vout, seq: t.seq}
		}
		s.forgetToken(ch, t)
	}
	s.refold(ch)
}

// firstMined orders tokens first mined first: lower height, then lower
// position in its block, then txid.
func firstMined(a, b *tokenEntry) bool {
	if a.height != b.height {
		return a.height < b.height
	}
	if a.offset != b.offset {
		return a.offset < b.offset
	}
	return a.txid < b.txid
}

// answerToken is the highest token that is not pending, the first mined
// among several.
func (s *Service) answerToken(ch *chainIdx) *tokenEntry {
	var best *tokenEntry
	for _, t := range ch.byTxid {
		if s.tokenStatusIn(ch, t) == borgrec.StatusPending {
			continue
		}
		if best == nil || t.seq > best.seq || (t.seq == best.seq && firstMined(t, best)) {
			best = t
		}
	}
	return best
}

// pathOf is the token path from t down to seq 0, t first.
func (s *Service) pathOf(ch *chainIdx, t *tokenEntry) []*tokenEntry {
	var out []*tokenEntry
	for cur := t; cur != nil; {
		out = append(out, cur)
		if cur.seq == 0 {
			break
		}
		cur = ch.byTxid[cur.prev]
	}
	return out
}

// foldAt is the fold after t (not pending), folded again from seq 0 when
// it was not kept.
func (s *Service) foldAt(ch *chainIdx, t *tokenEntry) chainFold {
	if kept := ch.states[t.txid]; kept != nil {
		return kept
	}
	path := s.pathOf(ch, t)
	f := ch.newFold()
	for i := len(path) - 1; i >= 0; i-- {
		f.apply(path[i])
	}
	return f
}

// answered is what the roll and policy answers use: the seq of the token
// they are after, its set, and the token path from it down to seq 0.
type answered struct {
	seq  uint64
	set  [][32]byte
	has  map[string]bool
	path []*tokenEntry
}

func newAnswered(seq uint64, set [][32]byte, path []*tokenEntry) *answered {
	a := &answered{seq: seq, set: set, has: make(map[string]bool, len(set)), path: path}
	for _, c := range set {
		a.has[hex.EncodeToString(c[:])] = true
	}
	return a
}

// answerRoll is the roll the answers use for an organization, with the seq
// of the token it is after.
func (s *Service) answerRoll(hall, org string) *answered {
	ch := s.chains[chainKey(hall, org, "")]
	if ch == nil {
		return nil
	}
	t := s.answerToken(ch)
	if t == nil {
		return nil
	}
	return newAnswered(t.seq, s.foldAt(ch, t).(orgFold).f.Roll(), s.pathOf(ch, t))
}

// answerPolicy is the policy set of the organization's answerable token,
// the set a role, bound or policy answer is taken from and the root its
// context proves against.
func (s *Service) answerPolicy(hall, org string) *answered {
	ch := s.chains[chainKey(hall, org, "")]
	if ch == nil {
		return nil
	}
	t := s.answerToken(ch)
	if t == nil {
		return nil
	}
	return newAnswered(t.seq, s.foldAt(ch, t).(orgFold).f.Policy(), s.pathOf(ch, t))
}

// ---- admission

// AdmissionMode implements module.LookupService: the whole transaction, as
// a carrier's funding outpoint, a token's predecessor and a sweep's spends
// are its inputs.
func (*Service) AdmissionMode() bmodule.AdmissionMode { return bmodule.AdmitWholeTx }

// SpendNotificationMode implements module.LookupService: spends are read
// from admitted transactions, never from the engine's notices.
func (*Service) SpendNotificationMode() bmodule.SpendNotificationMode { return bmodule.SpendNone }

func (s *Service) hallOf(topic string) (string, bool) {
	if !strings.HasPrefix(topic, borgrec.TopicPrefix) {
		return "", false
	}
	h := topic[len(borgrec.TopicPrefix):]
	return h, s.halls[h]
}

// parse reads an Atomic BEEF as the engine hands or stores it: its subject
// transaction.
func parse(raw []byte) (*transaction.Transaction, error) {
	_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errors.New("no subject transaction")
	}
	return tx, nil
}

// parseFor is the transaction txid in a stored BEEF, with its proof, as
// the TypeScript module's Transaction.fromBEEF(beef, txid) reads it.
func parseFor(raw []byte, txid chainhash.Hash) (*transaction.Transaction, error) {
	beef, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil {
		return nil, err
	}
	if beef != nil {
		if t := beef.FindAtomicTransactionByHash(&txid); t != nil {
			return t, nil
		}
	}
	if tx != nil && tx.TxID().Equal(txid) {
		return tx, nil
	}
	return nil, fmt.Errorf("no transaction %s in the BEEF", txid)
}

func srcTxid(in *transaction.TransactionInput) string {
	if in.SourceTXID != nil {
		return in.SourceTXID.String()
	}
	if in.SourceTransaction != nil {
		return in.SourceTransaction.TxID().String()
	}
	return ""
}

// place is a proof's block height and the txid's position in its block.
func place(tx *transaction.Transaction) (uint32, uint64) {
	mp := tx.MerklePath
	if mp == nil {
		return 0, 0
	}
	txid := tx.TxID()
	var offset uint64
	if len(mp.Path) > 0 {
		for _, l := range mp.Path[0] {
			if l != nil && l.Txid != nil && *l.Txid && l.Hash != nil && l.Hash.IsEqual(txid) {
				offset = l.Offset
				break
			}
		}
	}
	return mp.BlockHeight, offset
}

// OutputAdmittedByTopic implements module.LookupService: it indexes an
// admitted output, then reads back tokens whose predecessor is decided now
// and deletes what was dropped from storage.
func (s *Service) OutputAdmittedByTopic(ctx context.Context, p *bmodule.OutputAdmittedByTopic) error {
	if p.Mode != bmodule.AdmitWholeTx {
		return nil
	}
	hall, ok := s.hallOf(p.Topic)
	if !ok {
		return nil
	}
	tx, err := parse(p.AtomicBEEF)
	if err != nil {
		s.host.Log("ls_borg ignored an admission whose BEEF did not parse", map[string]any{"topic": p.Topic})
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index(hall, tx, p.Outpoint.Index, 0)
	s.enforceTable()
	s.drain(ctx)
	s.flush(ctx)
	return nil
}

// drain reads back, as the engine stores them, the tokens a full seq
// turned away whose predecessor has since been decided.
func (s *Service) drain(ctx context.Context) {
	if s.storage == nil {
		return
	}
	for len(s.reread) > 0 {
		r := s.reread[0]
		s.reread = s.reread[1:]
		if s.holdsToken(r.txid) {
			continue
		}
		h, err := chainhash.NewHashFromHex(r.txid)
		if err != nil {
			continue
		}
		o, err := s.storage.FindOutput(ctx, &transaction.Outpoint{Txid: *h, Index: r.vout}, borgrec.Topic(r.hall))
		if err != nil || o == nil || o.Beef == nil {
			continue
		}
		tx, err := parseFor(o.Beef, *h)
		if err != nil {
			s.host.Log("ls_borg could not read a token again", map[string]any{"txid": r.txid, "err": err.Error()})
			continue
		}
		s.index(r.hall, tx, r.vout, o.Score)
	}
}

// OutputEvicted implements module.LookupService.
func (s *Service) OutputEvicted(_ context.Context, op *transaction.Outpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(op.Txid.String(), op.Index, "")
	return nil
}

// OutputNoLongerRetainedInHistory implements module.RetentionListener.
func (s *Service) OutputNoLongerRetainedInHistory(_ context.Context, op *transaction.Outpoint, topic string) error {
	hall, ok := s.hallOf(topic)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(op.Txid.String(), op.Index, hall)
	return nil
}

// index indexes output vout of an admitted transaction on hall, writing its
// rows the first time it is seen. A carrier the rows say was dropped is not
// indexed again, and its output is deleted. It returns whether it indexed.
func (s *Service) index(hall string, tx *transaction.Transaction, vout uint32, score float64) bool {
	if int(vout) >= len(tx.Outputs) || tx.Outputs[vout] == nil || tx.Outputs[vout].LockingScript == nil {
		return false
	}
	// A held sweep whose tombstone this transaction spends leaves the
	// unspent rows storage hands a restore: the journal names it from now on.
	for _, in := range tx.Inputs {
		src := srcTxid(in)
		if src == "" || in.SourceTxOutIndex != 0 {
			continue
		}
		if sw := s.sweeps[src]; sw != nil && sw.halls[hall] && !s.spentSweeps[src][hall] {
			s.write(Line{K: "w", Txid: src, Hall: hall})
		}
	}
	script := []byte(*tx.Outputs[vout].LockingScript)
	txid := tx.TxID().String()
	switch kind := borgrec.Classify(script); kind {
	case borgrec.KindGrant, borgrec.KindPolicy, borgrec.KindWrap:
		return s.indexCarrier(hall, tx, kind, script)
	case borgrec.KindOrg, borgrec.KindGroup:
		return s.indexToken(hall, tx, vout)
	case borgrec.KindFunding:
		if vout == 0 {
			return s.indexSweep(hall, tx, score)
		}
	}
	s.host.Log("ls_borg indexed an output of no known kind", map[string]any{"hall": hall, "txid": txid, "vout": vout})
	return false
}

func (s *Service) indexCarrier(hall string, tx *transaction.Transaction, kind borgrec.Kind, script []byte) bool {
	if len(tx.Inputs) == 0 {
		return false
	}
	in := tx.Inputs[0]
	src := srcTxid(in)
	if src == "" {
		return false
	}
	op := outpoint(src, in.SourceTxOutIndex)
	h := tx.TxID()
	txid := h.String()
	c := hex.EncodeToString(h[:])
	record, ok := pushdrop.FirstPush(script)
	if !ok {
		return false
	}
	e := &carrierEntry{c: c, txid: txid, hall: hall, op: op}
	switch kind {
	case borgrec.KindGrant:
		g, err := borgrec.DecodeGrant(record)
		if err != nil {
			s.host.Log("ls_borg could not read a carrier", map[string]any{"hall": hall, "txid": txid, "err": err.Error()})
			return false
		}
		e.kind, e.org, e.subject = "g", hex.EncodeToString(g.Org), hex.EncodeToString(g.Subject)
	case borgrec.KindPolicy:
		p, err := borgrec.DecodePolicy(record)
		if err != nil {
			s.host.Log("ls_borg could not read a carrier", map[string]any{"hall": hall, "txid": txid, "err": err.Error()})
			return false
		}
		e.kind, e.org, e.role = "p", hex.EncodeToString(p.Org), p.Role
		if p.To != nil {
			e.to = hex.EncodeToString(p.To)
		}
	default:
		w, err := borgrec.DecodeWrap(record)
		if err != nil {
			s.host.Log("ls_borg could not read a carrier", map[string]any{"hall": hall, "txid": txid, "err": err.Error()})
			return false
		}
		e.kind, e.org, e.group, e.epoch = "w", hex.EncodeToString(w.Org), hex.EncodeToString(w.Group[:]), w.Epoch
		for _, r := range w.Recipients() {
			e.recipients = append(e.recipients, hex.EncodeToString(r))
		}
	}
	// Refuse before writing, for both reasons a carrier is not kept: a
	// dropped carrier re-offered must not add a line each time it is
	// offered, and one the row will not keep must not be written only to be
	// forgotten.
	if r := s.rows[op]; r != nil && r.dropped[c] {
		s.queueDelete(txid, 0, hall)
		return false
	}
	for _, x := range s.beyondBound(op, c, e.kind) {
		if x == c {
			s.host.Log("ls_borg refused a carrier past the evidence bound", map[string]any{"txid": txid, "outpoint": op})
			s.forgetForEvidence(op, c, txid, hall)
			return false
		}
	}
	if s.lines[c] == nil {
		s.write(Line{K: "c", Op: op, C: c, Txid: txid, Kind: e.kind, Hall: hall, At: i64(s.unix())})
		// Spends seen before any carrier used the outpoint join its row.
		if early := s.table.remove(op); early != nil {
			for _, sid := range early.order {
				sp := early.spends[sid]
				s.write(Line{K: "s", Op: op, S: sid, Hall: sp.hall, H: i64(sp.h), At: i64(sp.at)})
				s.host.Metrics().Inc("borg_retractions_total", nil, 1)
			}
		}
	}
	if old := s.carriers[c]; old != nil {
		e.n = old.n
	} else {
		s.carrierN++
		e.n = s.carrierN
	}
	s.carriers[c] = e
	s.carrierByTxid[txid] = c
	switch e.kind {
	case "g":
		addTo(s.grantsBySubject, hall+"|"+e.org+"|"+e.subject, c)
		addTo(s.grantsByOrg, hall+"|"+e.org, c)
	case "p":
		addTo(s.policiesByOrg, hall+"|"+e.org, c)
		addTo(s.policiesByRole, hall+"|"+e.org+"|"+e.role, c)
		if e.to != "" {
			addTo(s.policiesByTo, hall+"|"+e.org+"|"+e.to, c)
		}
	default:
		ep := strconv.FormatUint(e.epoch, 10)
		addTo(s.wrapsByEpoch, hall+"|"+e.org+"|"+e.group+"|"+ep, c)
		for _, m := range e.recipients {
			addTo(s.wrapsByMember, hall+"|"+e.org+"|"+e.group+"|"+ep+"|"+m, c)
		}
	}
	s.enforceEvidence(op)
	return true
}

func (s *Service) indexToken(hall string, tx *transaction.Transaction, vout uint32) bool {
	h := tx.TxID()
	txid := h.String()
	t, err := borgrec.ReadTokenOutput(tx.Outputs[vout], vout)
	if err != nil {
		s.host.Log("ls_borg could not read a token", map[string]any{"hall": hall, "txid": txid, "vout": vout})
		return false
	}
	var org, group, kind string
	var seq uint64
	var prev [32]byte
	if t.Org != nil {
		org, kind, seq, prev = hex.EncodeToString(t.Org.Org), "org", t.Org.Seq, t.Org.Prev
	} else {
		org, group, kind, seq, prev = hex.EncodeToString(t.Group.Org), hex.EncodeToString(t.Group.Group[:]), "group", t.Group.Seq, t.Group.Prev
	}
	key := chainKey(hall, org, group)
	ch := s.chains[key]
	if ch == nil {
		ch = &chainIdx{kind: kind, hall: hall, org: org, group: group, bySeq: map[uint64][]*tokenEntry{}, counts: map[string]string{},
			byTxid: map[string]*tokenEntry{}, children: map[string]map[string]bool{}, status: map[string]string{}, states: map[string]chainFold{},
			turned: map[string]map[string]turnedToken{}}
		s.chains[key] = ch
		if group != "" {
			addTo(s.groupsByOrg, hall+"|"+org, group)
		}
	}
	if _, seen := s.tokenSeen[txid]; !seen {
		s.write(Line{K: "t", Txid: txid, Vout: i64(int64(vout)), Hall: hall, At: i64(s.unix())})
	}
	if ch.byTxid[txid] != nil {
		return true
	}
	height, offset := place(tx)
	e := &tokenEntry{txid: txid, vout: vout, kind: ch.kind, hall: hall, org: org, group: group, chain: key, seq: seq, orgRec: t.Org, grpRec: t.Group,
		height: height, offset: offset}
	if seq > 0 {
		e.prev = chainhash.Hash(prev).String()
	}
	list := append(append([]*tokenEntry(nil), ch.bySeq[seq]...), e)
	sort.Slice(list, func(i, j int) bool { return list[i].txid < list[j].txid })
	ch.bySeq[seq] = list
	ch.byTxid[txid] = e
	if e.prev != "" {
		addTo(ch.children, e.prev, txid)
	}
	if s.restoring {
		return true
	}
	s.refold(ch)
	// The seq's bound is applied with every status known, so the set kept
	// does not depend on arrival order.
	s.bound(ch, seq)
	return ch.byTxid[txid] != nil
}

func (s *Service) forgetToken(ch *chainIdx, t *tokenEntry) {
	delete(ch.byTxid, t.txid)
	list := ch.bySeq[t.seq][:0:0]
	for _, x := range ch.bySeq[t.seq] {
		if x.txid != t.txid {
			list = append(list, x)
		}
	}
	if len(list) == 0 {
		delete(ch.bySeq, t.seq)
	} else {
		ch.bySeq[t.seq] = list
	}
	if t.prev != "" {
		removeFrom(ch.children, t.prev, t.txid)
	}
	ch.status = map[string]string{}
	ch.states = map[string]chainFold{}
}

func (s *Service) indexSweep(hall string, tx *transaction.Transaction, score float64) bool {
	txid := tx.TxID().String()
	e := s.sweeps[txid]
	if e == nil {
		height, offset := place(tx)
		var spent []string
		for _, in := range tx.Inputs {
			src := srcTxid(in)
			if src == "" {
				continue
			}
			op := outpoint(src, in.SourceTxOutIndex)
			spent = append(spent, op)
			if r := s.rows[op]; r != nil {
				if _, ok := r.sweeps[txid]; !ok {
					s.write(Line{K: "s", Op: op, S: txid, Hall: hall, H: i64(int64(height)), At: i64(s.unix())})
					s.host.Metrics().Inc("borg_retractions_total", nil, 1)
				}
			} else {
				te := s.table.get(op)
				if te == nil {
					te = s.table.add(op)
				}
				if _, ok := te.spends[txid]; !ok {
					te.spends[txid] = spend{hall: hall, h: int64(height), at: s.unix()}
					te.order = append(te.order, txid)
				}
			}
		}
		e = &sweepEntry{txid: txid, halls: map[string]bool{}, spent: spent, height: height, offset: offset, score: score}
		s.sweeps[txid] = e
	}
	// The same mined sweep admitted in another hall answers there too.
	if !e.halls[hall] {
		e.halls[hall] = true
		for _, op := range e.spent {
			addTo(s.sweepsByOp, hall+"|"+op, txid)
		}
	}
	return true
}

// sweepLive is whether a held sweep still records a spend: a row's, or one
// in the table.
func (s *Service) sweepLive(e *sweepEntry) bool {
	for _, op := range e.spent {
		if r := s.rows[op]; r != nil {
			if _, ok := r.sweeps[e.txid]; ok {
				return true
			}
		}
		if s.table.has(op, e.txid) {
			return true
		}
	}
	return false
}

// enforceTable keeps the table at its bound, oldest outpoint first. A sweep
// that then records no spend at all is dropped and deleted from storage.
func (s *Service) enforceTable() {
	for s.table.size() > s.tableCap {
		te := s.table.oldest()
		s.table.remove(te.op)
		for _, sid := range te.order {
			e := s.sweeps[sid]
			if e == nil || s.sweepLive(e) {
				continue
			}
			for _, h := range sortedKeys(e.halls) {
				s.queueDelete(sid, 0, h)
			}
			s.forget(sid, 0, "")
			s.host.Metrics().Inc("borg_dropped_total", map[string]string{"why": "table"}, 1)
		}
	}
}

// forget removes a held output from the index, in one hall or in all; its
// rows stay.
func (s *Service) forget(txid string, vout uint32, hall string) {
	if vout == 0 {
		if c, ok := s.carrierByTxid[txid]; ok {
			if e := s.carriers[c]; e != nil && (hall == "" || e.hall == hall) {
				s.forgetCarrier(e)
			}
		}
		if sw := s.sweeps[txid]; sw != nil {
			halls := []string{hall}
			if hall == "" {
				halls = sortedKeys(sw.halls)
			}
			for _, h := range halls {
				if !sw.halls[h] {
					continue
				}
				delete(sw.halls, h)
				for _, op := range sw.spent {
					removeFrom(s.sweepsByOp, h+"|"+op, txid)
				}
			}
			if len(sw.halls) == 0 {
				delete(s.sweeps, txid)
			}
		}
	}
	for _, ch := range s.chains {
		if hall != "" && ch.hall != hall {
			continue
		}
		if t := ch.byTxid[txid]; t != nil && t.vout == vout {
			s.forgetToken(ch, t)
		}
	}
}

func (s *Service) forgetCarrier(e *carrierEntry) {
	delete(s.carriers, e.c)
	delete(s.carrierByTxid, e.txid)
	switch e.kind {
	case "g":
		removeFrom(s.grantsBySubject, e.hall+"|"+e.org+"|"+e.subject, e.c)
		removeFrom(s.grantsByOrg, e.hall+"|"+e.org, e.c)
	case "p":
		removeFrom(s.policiesByOrg, e.hall+"|"+e.org, e.c)
		removeFrom(s.policiesByRole, e.hall+"|"+e.org+"|"+e.role, e.c)
		if e.to != "" {
			removeFrom(s.policiesByTo, e.hall+"|"+e.org+"|"+e.to, e.c)
		}
	default:
		ep := strconv.FormatUint(e.epoch, 10)
		removeFrom(s.wrapsByEpoch, e.hall+"|"+e.org+"|"+e.group+"|"+ep, e.c)
		for _, m := range e.recipients {
			removeFrom(s.wrapsByMember, e.hall+"|"+e.org+"|"+e.group+"|"+ep+"|"+m, e.c)
		}
	}
}

// ---- drops and retention (spec section 11.3)

// drop drops the carrier with commitment c for a reason that must stick:
// the journal records it, so the carrier is never admitted again after a
// restart. Retention is such a reason, because re-admitting a grant 31
// days after it left the roll would undo retention.
func (s *Service) drop(c, why string) {
	l := s.lines[c]
	if l == nil {
		return
	}
	r := s.rows[l.Op]
	if r.dropped[c] {
		return
	}
	op, txid, hall := l.Op, l.Txid, l.Hall
	s.write(Line{K: "d", Op: op, C: c, W: boolp(s.winnerOf(op) == c)})
	s.forget(txid, 0, "")
	s.queueDelete(txid, 0, hall)
	s.host.Metrics().Inc("borg_dropped_total", map[string]string{"why": why}, 1)
	s.host.Log("ls_borg dropped a carrier", map[string]any{"txid": txid, "outpoint": op, "why": why})
}

// forgetForEvidence forgets a carrier the evidence bound does not keep:
// the output is deleted and the line is not kept, and nothing is written,
// because the refusal is decided by the row and not by memory of this
// carrier. Offered again, it meets the same row and is refused again, so a
// journal line would buy nothing and the journal does not grow with what
// an equivocating organization offers.
func (s *Service) forgetForEvidence(op, c, txid, hall string) {
	r := s.rowOf(op)
	if _, ok := r.carriers[c]; ok {
		delete(r.carriers, c)
		delete(s.lines, c)
		s.forget(txid, 0, "")
	}
	r.more++
	s.queueDelete(txid, 0, hall)
	s.host.Metrics().Inc("borg_dropped_total", map[string]string{"why": "evidence"}, 1)
}

// beyondBound are the commitments an outpoint's row would stop keeping once
// c is on it, highest first: the superseded carriers past the kind's
// bound. It is a pure read of the row, so the same answer is reached before
// a line is written and after, and so a carrier the row will not keep is
// refused without being written at all.
func (s *Service) beyondBound(op, c, kind string) []string {
	r := s.rows[op]
	var all []string
	in := false
	if r != nil {
		for x := range r.carriers {
			all = append(all, x)
			in = in || x == c
		}
	}
	if !in {
		all = append(all, c)
	}
	sort.Strings(all)
	// The winner is held, not superseded, unless the row is closed: then
	// every carrier on it is superseded (statusOf).
	superseded := all
	if r == nil || !r.closed {
		superseded = all[1:]
	}
	limit := EvidenceGrants
	if kind == "w" {
		limit = EvidenceWraps
	}
	if len(superseded) <= limit {
		return nil
	}
	out := append([]string(nil), superseded[limit:]...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// enforceEvidence keeps at most 8 superseded wraps, or 64 superseded grants
// or policy records, on an outpoint, and drops the rest, highest
// commitment first.
func (s *Service) enforceEvidence(op string) {
	r := s.rows[op]
	if r == nil {
		return
	}
	var kept []*Line
	for _, l := range r.carriers {
		if !r.dropped[l.C] && s.statusOf(l.C) == borgrec.StatusSuperseded {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return
	}
	limit := EvidenceGrants
	if kept[0].Kind == "w" {
		limit = EvidenceWraps
	}
	order := make([]string, 0, len(kept))
	for _, l := range kept {
		order = append(order, l.C)
	}
	sort.Strings(order)
	if len(order) <= limit {
		return
	}
	past := order[limit:]
	for i := len(past) - 1; i >= 0; i-- {
		c := past[i]
		l := s.lines[c]
		if l == nil {
			continue
		}
		s.host.Log("ls_borg forgot a carrier past the evidence bound", map[string]any{"txid": l.Txid, "outpoint": op})
		s.forgetForEvidence(op, c, l.Txid, l.Hall)
	}
}

// carriersInOrder are the held carriers in the order the host first
// indexed them.
func (s *Service) carriersInOrder() []*carrierEntry {
	out := make([]*carrierEntry, 0, len(s.carriers))
	for _, e := range s.carriers {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out
}

// Prune applies retention at the host's time and deletes what was dropped
// from storage. Kept: tokens and matched sweeps for the host's life; a
// grant while the answered roll holds it, one never in a roll until its
// outpoint is swept, and otherwise for the retention period after it left
// the roll or was retracted; a policy record likewise with the answered
// policy set; the wraps of each group's current epoch, and those of an
// earlier epoch for the retention period after the host first saw the
// token that ended it. It returns how many carriers were dropped.
func (s *Service) Prune(ctx context.Context) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.unix()
	old := func(from int64) bool { return now-from >= s.retention }
	n := 0
	rolls := map[string]*answered{}
	revokedAt := map[string]map[string]int64{}
	sets := map[string]*answered{}
	retiredAt := map[string]map[string]int64{}
	seenOr := func(txid string) int64 {
		if at, ok := s.tokenSeen[txid]; ok {
			return at
		}
		return now
	}
	for _, e := range s.carriersInOrder() {
		// A held carrier keeps its line: only a dropped one loses it, and a
		// dropped carrier is not in this map.
		l := s.lines[e.c]
		r := s.rows[e.op]
		if l == nil || r == nil {
			continue
		}
		var sweptAt int64
		swept := false
		for _, sp := range r.sweeps {
			if !swept || sp.at < sweptAt {
				sweptAt, swept = sp.at, true
			}
		}
		var from int64
		switch e.kind {
		case "g", "p":
			k := e.hall + "|" + e.org
			ans, at := rolls, revokedAt
			if e.kind == "p" {
				ans, at = sets, retiredAt
			}
			if _, done := ans[k]; !done {
				var a *answered
				if e.kind == "g" {
					a = s.answerRoll(e.hall, e.org)
				} else {
					a = s.answerPolicy(e.hall, e.org)
				}
				ans[k] = a
				m := map[string]int64{}
				// The path runs from the answered token down, so the first
				// change met is the latest.
				if a != nil {
					for _, t := range a.path {
						list := t.orgRec.Revoke
						if e.kind == "p" {
							list = t.orgRec.Retire
						}
						for _, c := range list {
							ch := hex.EncodeToString(c[:])
							if _, ok := m[ch]; !ok {
								m[ch] = seenOr(t.txid)
							}
						}
					}
				}
				at[k] = m
			}
			if a := ans[k]; a != nil && a.has[e.c] {
				continue
			}
			left, isLeft := at[k][e.c]
			if !swept && !isLeft {
				continue
			}
			from = l.at()
			if swept && sweptAt > from {
				from = sweptAt
			}
			if isLeft && left > from {
				from = left
			}
		default:
			ch := s.chains[chainKey(e.hall, e.org, e.group)]
			switch {
			case swept:
				from = l.at()
				if sweptAt > from {
					from = sweptAt
				}
			case ch == nil:
				from = l.at()
			default:
				latest := int64(-1)
				var ended int64
				hasEnded := false
				for _, t := range ch.byTxid {
					ep := int64(t.grpRec.Epoch) //nolint:gosec // an epoch is at most 2^32 - 1
					if ep > latest {
						latest = ep
					}
					if t.grpRec.Epoch > e.epoch {
						seen := seenOr(t.txid)
						if !hasEnded || seen < ended {
							ended, hasEnded = seen, true
						}
					}
				}
				if int64(e.epoch) >= latest || !hasEnded { //nolint:gosec // an epoch is at most 2^32 - 1
					continue
				}
				from = l.at()
				if ended > from {
					from = ended
				}
			}
		}
		if old(from) {
			s.drop(e.c, "retention")
			n++
		}
	}
	s.flush(ctx)
	return n
}

// flush deletes dropped outputs from storage. It runs after every
// admission and after retention, so a dropped output is gone at once. A
// store that cannot delete keeps what was dropped; the queue is not kept
// for it.
func (s *Service) flush(ctx context.Context) {
	if s.storage == nil || len(s.pending) == 0 {
		return
	}
	del, ok := s.storage.(Deleter)
	if !ok {
		if !s.warnedNoDelete {
			s.host.Log("ls_borg cannot delete dropped outputs: the storage has no deleteOutput", map[string]any{})
		}
		s.warnedNoDelete = true
		s.pending = map[string]pendingDelete{}
		return
	}
	keys := make([]string, 0, len(s.pending))
	for k := range s.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := s.pending[k]
		h, err := chainhash.NewHashFromHex(p.txid)
		if err != nil {
			delete(s.pending, k)
			continue
		}
		if err := del.DeleteOutput(ctx, &transaction.Outpoint{Txid: *h, Index: p.vout}, p.topic); err != nil {
			s.host.Log("ls_borg could not delete a dropped output", map[string]any{"txid": p.txid, "err": err.Error()})
			continue
		}
		delete(s.pending, k)
	}
}

// Restored reports whether the index has been restored.
func (s *Service) Restored() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage != nil
}

func (s *Service) holdsToken(txid string) bool {
	for _, ch := range s.chains {
		if ch.byTxid[txid] != nil {
			return true
		}
	}
	return false
}

func (s *Service) tokenAt(txid string, vout uint32) *tokenEntry {
	for _, ch := range s.chains {
		if t := ch.byTxid[txid]; t != nil && t.vout == vout {
			return t
		}
	}
	return nil
}

// GetDocumentation implements module.LookupService.
func (*Service) GetDocumentation() string {
	return strings.Join([]string{
		"# ls_borg",
		"",
		"Answers every hall this host carries (spec section 10.2). Free classes: `org`,",
		"`org-seq`, `group`, `group-seq` (a chain's latest tokens or those at one seq, at",
		"most 8), `roll`, `roll-after` (the grant carriers of an organization's roll, 64 a",
		"page), `member` (a key's grants in the roll, each with its inclusion path as",
		"context), `groups`, `groups-after`, `member-of`, `member-of-after` (the latest",
		"token of each group), `wraps`, `wraps-after`, `wrap` (an epoch's wrap carriers)",
		"and `sweep` (sweeps spending an outpoint). Priceable: `history`, `history-after`",
		"(every token of a chain, by seq). A question must carry exactly the members of",
		"one class; anything else is refused with an error. An empty answer means only",
		"that this host holds nothing that answers.",
	}, "\n")
}

// GetMetaData implements module.LookupService.
func (*Service) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{Name: borgrec.LookupService, Description: "Answers borg organizations, groups, grants, wraps and sweeps by hall."}
}
