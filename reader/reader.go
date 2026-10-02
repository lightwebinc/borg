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
}

// Query is one ls_borg question: its members, every one a string.
type Query map[string]string

// ErrHost is a host that answered with an error: its words travel with it.
var ErrHost = errors.New("reader: the host refused the question")

// ErrNoHeaders refuses to verify against no header source.
var ErrNoHeaders = errors.New("reader: no header source; refusing to verify against a default")

// Ask asks one host one question and returns the outputs it answered.
func (c *Client) Ask(ctx context.Context, host string, q Query) ([]lookup.Output, error) {
	hs := &hostset.Client{Timeout: c.Timeout, HTTP: c.HTTP}
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
	for _, o := range outs {
		if SubjectID(o.Beef) == txid {
			return true, nil
		}
	}
	return false, nil
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

// HostOutputs is what one host answered to one question, all pages.
type HostOutputs struct {
	Host      string
	Outputs   []lookup.Output
	Err       error
	Truncated bool
}

// Pages asks one host a paged class, page after page, until a page is
// short. cursor gives the -after member for an output, or "" when it has
// none (which ends the walk).
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
		ho.Outputs = append(ho.Outputs, outs...)
		if len(outs) < limits.PageSize || !afterClass {
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
