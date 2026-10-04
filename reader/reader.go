// Package reader asks overlay hosts the ls_borg questions and checks every
// answer itself (docs/spec.md sections 10.2, 12 and 13): a token's record,
// lock and field signature and its proof against the reader's own headers;
// a carrier by the host's own admission rules and by SPV through its funding
// parent; a sweep by its proof. A host is trusted for nothing: an answer is
// verified here or reported as refused, naming the host. An empty answer
// means only that the host holds nothing that answers.
//
// A reader that asks several hosts compares their answers and reports a
// difference as such, never merging it silently: a host can withhold a
// token, a grant or a wrap, and only a comparison (or a root) shows it.
//
// The package is importable by other modules at a tagged version
// (docs/packages.md names the supported part of it).
package reader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/hostset"
	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
)

// Client asks hosts and checks their answers.
type Client struct {
	// Hosts are the overlay hosts' base URLs, asked in order.
	Hosts []string
	// Headers is the reader's own header source. It is required: an answer
	// is only as good as the headers it is checked against.
	Headers chaintracker.ChainTracker
	// Timeout bounds each request; HTTP, when set, is the client used.
	Timeout time.Duration
	HTTP    *http.Client
	// MaxPages bounds the pages one question reads per host.
	MaxPages int
	// MaxAnswer bounds the bytes taken for one answer; zero is
	// DefaultMaxAnswer.
	MaxAnswer int64
	// Source, when set, is a chain source asked for a transaction's
	// current proof when the proof a host stored no longer verifies
	// against Headers (docs/spec.md section 11.5). Without one, such an
	// answer is refused as unmined.
	Source ChainSource
	// Floor, when set, is the caller's memory of the chains it verified:
	// Chain refuses hosts that answer an older state than it holds, and
	// records what it verified (floor.go). Without one a reader has no
	// memory, and a host set that withholds the latest tokens of a chain
	// is believed.
	Floor Floor
}

// DefaultMaxAnswer is the most bytes a reader takes for one lookup answer
// when Client.MaxAnswer is zero: a full page of the largest objects the
// plane carries fits several times over.
const DefaultMaxAnswer = limits.MaxAnswer

// ChainSource is a source of a mined transaction's current proof, by its
// txid: a node's asset API (nodeapi.Asset is one).
type ChainSource interface {
	MerkleProof(ctx context.Context, txid string) (*transaction.MerklePath, error)
}

// Query is one ls_borg question: its members, every one a string.
type Query map[string]string

// ErrHost is a host that answered with an error: its words travel with it.
var ErrHost = errors.New("reader: the host refused the question")

// ErrNoHeaders refuses to verify against no header source.
var ErrNoHeaders = errors.New("reader: no header source; refusing to verify against a default")

// Ask asks one host one question and returns the outputs it answered. An
// answer over MaxAnswer bytes is an error, not read.
func (c *Client) Ask(ctx context.Context, host string, q Query) ([]lookup.Output, error) {
	maxBody := c.MaxAnswer
	if maxBody <= 0 {
		maxBody = DefaultMaxAnswer
	}
	hs := &hostset.Client{Timeout: c.Timeout, HTTP: c.HTTP, MaxBody: maxBody}
	res, err := lookup.Query(ctx, hs, host, lookup.Question{Service: borgrec.LookupService, Query: q})
	if err != nil {
		if strings.Contains(err.Error(), "answered status 4") {
			return nil, fmt.Errorf("%w: %v", ErrHost, err)
		}
		return nil, err
	}
	var out []lookup.Output
	for _, r := range res {
		out = append(out, r.Answer.Outputs...)
	}
	return out, nil
}

// SubjectID is the txid of a BEEF's subject: the one an Atomic BEEF names
// in its header, else the one the BEEF parses to, else a note that there is
// none.
func SubjectID(beef []byte) string {
	if len(beef) >= 36 && binary.LittleEndian.Uint32(beef) == transaction.ATOMIC_BEEF {
		return chainhash.Hash(beef[4:36]).String()
	}
	_, _, id, err := transaction.ParseBeef(beef)
	if err != nil || id == nil {
		return "(no subject)"
	}
	return id.String()
}

// Held asks whether host answers a question with an output whose subject is
// txid: a unicast publisher's confirmation that a host that admitted
// nothing holds the object (spec section 12).
func (c *Client) Held(ctx context.Context, host string, q Query, txid string) (bool, error) {
	outs, err := c.Ask(ctx, host, q)
	if err != nil {
		return false, err
	}
	if len(outs) > pageOutputs {
		return false, overfull(len(outs))
	}
	for _, o := range outs {
		if SubjectID(o.Beef) == txid {
			return true, nil
		}
	}
	return false, nil
}

// ErrOverfull is an answer to one question that holds more outputs than a
// page of this contract may (pageOutputs): a host fault. Only the first
// pageOutputs are read, and the host is named.
var ErrOverfull = errors.New("reader: the host answered more outputs than a page holds")

// ErrTruncated is a paged answer the reader cut (Pages): what the host
// answered past the cut is not read.
var ErrTruncated = errors.New("reader: the host's answer was cut at the reader's bound (a page over the bound, a walk too long, or a host repeating itself); what it answered past the cut is not read")

func overfull(n int) error { return overBound(n, pageOutputs) }

func overBound(n, bound int) error {
	return fmt.Errorf("%w: %d outputs to one question, over the bound of %d; only %d are read", ErrOverfull, n, bound, bound)
}

// capAnswer bounds one unpaged answer at bound outputs: an answer over it
// is cut, and a Refusal names the host.
func capAnswer(host string, outs []lookup.Output, bound int) ([]lookup.Output, *Refusal) {
	if len(outs) <= bound {
		return outs, nil
	}
	return outs[:bound], &Refusal{Host: host, Txid: fmt.Sprintf("(%d outputs to one question)", len(outs)), Reason: "overfull", Err: overBound(len(outs), bound)}
}

// verdicts remembers what one call verified, by the exact bytes of each
// answered BEEF: the same bytes verify the same way against the same
// headers, so a copy is not verified again. A copy from a host already
// named for those bytes is skipped; from another host it takes the
// verdict, and a refusal names that host too.
type verdicts map[[32]byte]*verdict

type verdict struct {
	car   *Car
	err   error
	hosts map[string]bool
}

// carrier is Client.Carrier once per distinct BEEF. again is set when the
// bytes were already verified in this call, sameHost when this host
// answered them before.
func (v verdicts) carrier(ctx context.Context, c *Client, host string, beef []byte, hall string, want borgrec.TxKind) (car *Car, again, sameHost bool, err error) {
	k := sha256.Sum256(beef)
	if x := v[k]; x != nil {
		sameHost = x.hosts[host]
		x.hosts[host] = true
		return x.car, true, sameHost, x.err
	}
	car, err = c.Carrier(ctx, beef, hall, want)
	v[k] = &verdict{car: car, err: err, hosts: map[string]bool{host: true}}
	return car, false, false, err
}

// Refusal is an answered output that did not verify.
type Refusal struct {
	Host   string
	Txid   string
	Reason string
	Err    error
}

// Tok is one answered chain token that verified.
type Tok struct {
	Txid string
	Tx   *transaction.Transaction
	// Token is the token output, its record decoded.
	Token *borgrec.ChainToken
	// Height and Pos place it: its block and its offset in the block.
	Height uint32
	Pos    uint64
	// Beef is the token as answered (the engine's stored form).
	Beef []byte
}

// Seq is the token's seq.
func (t *Tok) Seq() uint64 { return t.Token.Seq() }

// Prev is the txid its record names as its predecessor's, display order.
func (t *Tok) Prev() string {
	var p [32]byte
	if t.Token.Org != nil {
		p = t.Token.Org.Prev
	} else {
		p = t.Token.Group.Prev
	}
	return chainhash.Hash(p).String()
}

// Before reports whether t was mined before u: lower height, then lower
// position in its block (spec section 13).
func (t *Tok) Before(u *Tok) bool {
	if t.Height != u.Height {
		return t.Height < u.Height
	}
	if t.Pos != u.Pos {
		return t.Pos < u.Pos
	}
	return t.Txid < u.Txid
}

// VerifyToken checks one answered token as a reader does (spec section 13):
// the transaction holds exactly one well-formed token (its record, its lock
// derived from the record's organisation, its field signature), of hall and
// organisation org, and its proof verifies against headers.
func VerifyToken(ctx context.Context, beef []byte, hall string, org []byte, headers chaintracker.ChainTracker) (*Tok, error) {
	if headers == nil {
		return nil, ErrNoHeaders
	}
	if len(beef) > limits.MaxBEEF {
		return nil, fmt.Errorf("%w: %d bytes", borgrec.ErrBEEF, len(beef))
	}
	tx, tok, err := borgrec.StoredToken(beef)
	if err != nil {
		return nil, err
	}
	var h string
	var o []byte
	if tok.Org != nil {
		h, o = tok.Org.Hall, tok.Org.Org
	} else {
		h, o = tok.Group.Hall, tok.Group.Org
	}
	if h != hall {
		return nil, fmt.Errorf("%w: the token names hall %q", borgrec.ErrHall, h)
	}
	if org != nil && !bytes.Equal(o, org) {
		return nil, errors.New("reader: the token is another organisation's")
	}
	if !borgrec.Mined(ctx, tx, headers) {
		return nil, borgrec.ErrUnmined
	}
	t := &Tok{Txid: tx.TxID().String(), Tx: tx, Token: tok, Height: tx.MerklePath.BlockHeight, Beef: beef}
	for _, l := range tx.MerklePath.Path[0] {
		if l.Hash != nil && *l.Hash == *tx.TxID() {
			t.Pos = l.Offset
		}
	}
	return t, nil
}

// Car is one answered carrier that verified.
type Car struct {
	Txid string
	Beef []byte
	Tx   *transaction.Transaction
	// Carrier is the carrier by the host's own rules: its record, owner,
	// commitment and funding outpoint.
	Carrier *borgrec.Carrier
	Kind    borgrec.TxKind
	// Context is the BRC-24 context the host answered with it.
	Context []byte
}

// VerifyCarrier checks one answered carrier: the host's admission rules
// for hall (every carrier rule and the grant's content rules), SPV through
// its funding parent against headers, and its kind.
func VerifyCarrier(ctx context.Context, beef []byte, hall string, want borgrec.TxKind, headers chaintracker.ChainTracker) (*Car, error) {
	if headers == nil {
		return nil, ErrNoHeaders
	}
	_, tx, txid, err := guard.ParseBEEF(beef, limits.MaxBEEF)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("%w: %v", borgrec.ErrBEEF, err)
	}
	a, err := borgrec.Admit(ctx, beef, nil, borgrec.Host{Hall: hall, Headers: headers, MaxBEEF: limits.MaxBEEF})
	if err != nil {
		return nil, err
	}
	if a.Kind != want || a.Carrier == nil {
		return nil, fmt.Errorf("reader: the answer is a %s, not a %s", a.Kind, want)
	}
	if ok, err := spv.Verify(ctx, tx, headers, nil); err != nil || !ok {
		return nil, fmt.Errorf("reader: SPV of the carrier through its funding parent: %v", err)
	}
	return &Car{Txid: txid.String(), Beef: beef, Tx: tx, Carrier: a.Carrier, Kind: a.Kind}, nil
}

// VerifySweep checks an answered sweep: admitted by the host's rules (a
// mined transaction whose output 0 is funding-shaped) against the reader's
// own headers, and returns the outpoints it spends.
func VerifySweep(ctx context.Context, beef []byte, hall string, headers chaintracker.ChainTracker) ([]transaction.Outpoint, string, error) {
	if headers == nil {
		return nil, "", ErrNoHeaders
	}
	a, err := borgrec.Admit(ctx, beef, nil, borgrec.Host{Hall: hall, Headers: headers, MaxBEEF: limits.MaxBEEF})
	if err != nil {
		return nil, "", err
	}
	if a.Kind != borgrec.TxSweep {
		return nil, "", fmt.Errorf("reader: the answer is a %s, not a sweep", a.Kind)
	}
	return a.Spent, chainhash.Hash(a.Txid).String(), nil
}

// A host stores an object with the proof it was given, and cannot take a
// new one: after a reorganisation the stored proof may name a block that is
// no longer in the best chain, although the same transaction was mined
// again elsewhere (spec section 11.5). A reader then fetches the
// transaction's current proof from a chain source by its txid, and verifies
// that against its own headers. The three methods below do it for a token,
// a carrier's funding tree and a sweep; without a chain source they are the
// plain checks.

// current is the chain source's proof of tx, cut to the minimal one, when
// the proof tx carries does not verify against the reader's headers and the
// source's does.
func (c *Client) current(ctx context.Context, tx *transaction.Transaction) bool {
	if c.Source == nil || tx == nil || c.Headers == nil {
		return false
	}
	if tx.MerklePath != nil {
		if ok, err := tx.MerklePath.Verify(ctx, tx.TxID(), c.Headers); err != nil || ok {
			return false
		}
	}
	mp, err := c.Source.MerkleProof(ctx, tx.TxID().String())
	if err != nil || mp == nil {
		return false
	}
	if mp, err = borgrec.MinimalPath(mp, tx.TxID()); err != nil {
		return false
	}
	if ok, err := mp.Verify(ctx, tx.TxID(), c.Headers); err != nil || !ok {
		return false
	}
	tx.MerklePath = mp
	return true
}

// Token is VerifyToken against the client's headers, with a stale stored
// proof replaced by the chain source's current one.
func (c *Client) Token(ctx context.Context, beef []byte, hall string, org []byte) (*Tok, error) {
	t, err := VerifyToken(ctx, beef, hall, org, c.Headers)
	if !errors.Is(err, borgrec.ErrUnmined) || len(beef) > limits.MaxBEEF {
		return t, err
	}
	tx, _, perr := borgrec.StoredToken(beef)
	if perr != nil || !c.current(ctx, tx) {
		return nil, err
	}
	again, perr := tx.AtomicBEEF(false)
	if perr != nil {
		return nil, err
	}
	return VerifyToken(ctx, again, hall, org, c.Headers)
}

// Carrier is VerifyCarrier against the client's headers, with a stale
// proof of the carrier's funding tree replaced by the chain source's
// current one.
func (c *Client) Carrier(ctx context.Context, beef []byte, hall string, want borgrec.TxKind) (*Car, error) {
	car, err := VerifyCarrier(ctx, beef, hall, want, c.Headers)
	if err == nil || c.Source == nil {
		return car, err
	}
	_, tx, _, perr := guard.ParseBEEF(beef, limits.MaxBEEF)
	if perr != nil || tx == nil || len(tx.Inputs) != 1 || !c.current(ctx, tx.Inputs[0].SourceTransaction) {
		return nil, err
	}
	again, perr := tx.AtomicBEEF(false)
	if perr != nil {
		return nil, err
	}
	return VerifyCarrier(ctx, again, hall, want, c.Headers)
}

// Sweep is VerifySweep against the client's headers, with a stale stored
// proof replaced by the chain source's current one.
func (c *Client) Sweep(ctx context.Context, beef []byte, hall string) ([]transaction.Outpoint, string, error) {
	spent, txid, err := VerifySweep(ctx, beef, hall, c.Headers)
	if !errors.Is(err, borgrec.ErrUnmined) {
		return spent, txid, err
	}
	_, tx, _, perr := guard.ParseBEEF(beef, limits.MaxBEEF)
	if perr != nil || tx == nil || !c.current(ctx, tx) {
		return nil, "", err
	}
	again, perr := tx.AtomicBEEF(false)
	if perr != nil {
		return nil, "", err
	}
	return VerifySweep(ctx, again, hall, c.Headers)
}

// HostOutputs is what one host answered to one question, all pages.
type HostOutputs struct {
	Host      string
	Outputs   []lookup.Output
	Err       error
	Truncated bool
}

// pageOutputs is the most outputs taken from one page (a page of this
// contract is at most limits.PageSize), and walkBytes the most bytes one
// paged walk holds from one host.
const (
	pageOutputs = 2 * limits.PageSize
	walkBytes   = 256 << 20
)

// Pages asks one host a paged class, page after page, until a page is
// short. cursor gives the -after member for an output, or "" when it has
// none (which ends the walk). What one page and one walk hold is bounded
// here, whatever a host answers: a page over the contract's is cut, a walk
// over walkBytes stops, and a host that answers nothing past the cursor it
// was given ends the walk. Each is reported as Truncated, never
// accumulated.
func (c *Client) Pages(ctx context.Context, host string, q Query, afterClass bool, cursor func(lookup.Output) string) HostOutputs {
	ho := HostOutputs{Host: host}
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = limits.MaxPages
	}
	page := Query{}
	for k, v := range q {
		page[k] = v
	}
	held := 0
	for n := 0; ; n++ {
		if n == maxPages {
			ho.Truncated = true
			return ho
		}
		outs, err := c.Ask(ctx, host, page)
		if err != nil {
			ho.Err = err
			return ho
		}
		full := len(outs) >= limits.PageSize
		if len(outs) > pageOutputs {
			outs, ho.Truncated = outs[:pageOutputs], true
		}
		for _, o := range outs {
			held += len(o.Beef) + len(o.Context)
		}
		if held > walkBytes {
			ho.Truncated = true
			return ho
		}
		ho.Outputs = append(ho.Outputs, outs...)
		if ho.Truncated || !full || !afterClass {
			return ho
		}
		last := ""
		for _, o := range outs {
			if k := cursor(o); k > last {
				last = k
			}
		}
		if last == "" {
			return ho
		}
		if prev, ok := page["after"]; ok && last <= prev {
			// A full page that does not move past the cursor it was asked
			// after: the host is repeating itself.
			ho.Truncated = true
			return ho
		}
		page["after"] = last
	}
}

// Hash is a display txid as a hash (hash byte order).
func Hash(txid string) ([32]byte, error) {
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil || h.String() != txid {
		return [32]byte{}, fmt.Errorf("%q is not a transaction id (64 lowercase hex characters)", txid)
	}
	return [32]byte(*h), nil
}

// Terms is a host's terms document (spec section 10.3).
type Terms struct {
	Service string `json:"service"`
	Terms   int    `json:"terms"`
	Classes []struct {
		Class    string `json:"class"`
		Satoshis uint64 `json:"satoshis"`
	} `json:"classes"`
}

// ErrNoTerms is a host that serves no terms document: it charges for
// nothing.
var ErrNoTerms = errors.New("reader: the host serves no terms document, so it charges for nothing")

// FetchTerms reads <base>/ls_borg/terms. Members and classes a client does
// not know are ignored; a free class with a price is reported, and a client
// never pays for one.
func FetchTerms(ctx context.Context, hc *http.Client, base string) (*Terms, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+borgrec.LookupService+"/terms", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNoTerms
	default:
		return nil, fmt.Errorf("terms: status %d", resp.StatusCode)
	}
	var t Terms
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("terms: not the terms document: %w", err)
	}
	if t.Service != borgrec.LookupService || t.Terms != 1 {
		return nil, fmt.Errorf("terms: service %q version %d, not ls_borg version 1", t.Service, t.Terms)
	}
	return &t, nil
}

// GroupHex is a group id as a question writes it.
func GroupHex(g [borgrec.GroupIDLen]byte) string { return hex.EncodeToString(g[:]) }

// ParseGroup reads a group id from 32 lowercase hex digits.
func ParseGroup(s string) ([borgrec.GroupIDLen]byte, error) {
	var g [borgrec.GroupIDLen]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != borgrec.GroupIDLen || hex.EncodeToString(b) != s {
		return g, fmt.Errorf("%q is not a group id (32 lowercase hex digits)", s)
	}
	copy(g[:], b)
	return g, nil
}

// Seq is a seq as a question writes it.
func Seq(n uint64) string { return strconv.FormatUint(n, 10) }

// sortToks orders tokens by seq then txid, the answer order.
func sortToks(ts []*Tok) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Seq() != ts[j].Seq() {
			return ts[i].Seq() < ts[j].Seq()
		}
		return ts[i].Txid < ts[j].Txid
	})
}
