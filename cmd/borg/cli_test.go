package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/borg/internal/config"

	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/testchain"
)

func init() {
	poll = 5 * time.Millisecond
	engine.Retries = []time.Duration{time.Millisecond}
	limits.Retries = engine.Retries
	limits.PlaneWait = []time.Duration{time.Millisecond}
	// A history page of more than 2 tokens is followed by another, so that
	// a chain of a few tokens takes several pages.
	historyLast = 2
}

// harness is a local chain, two stand-in hosts behind a stand-in plane, a
// terms route on host a, and the environment that points the command at
// them. Each identity is a home of its own.
type harness struct {
	t      *testing.T
	dir    string
	chain  *testchain.Chain
	a, b   *testchain.Host
	chainS *httptest.Server
	aS, bS *httptest.Server
	planeS *httptest.Server
	paid   *testchain.Paid
	paidS  *httptest.Server
	plane  *testchain.Plane
	env    map[string]string
	hall   string

	mu sync.Mutex
	// onSend, when set, runs once, before the chain takes the next
	// transaction a client sends, with that transaction.
	onSend func(tx *transaction.Transaction)
	// refuse, when set, is asked for every such transaction; true answers
	// the client with an error and keeps the transaction from the chain.
	refuse func(tx *transaction.Transaction) bool
	// pruned are the transactions the node no longer answers, as a node
	// that drops fully spent transactions: neither their bytes nor a
	// proof.
	pruned map[string]bool
	// unknown are the transactions whose outputs the node's UTXO view
	// answers with status NOT_FOUND, as a node does for outputs it has
	// pruned or could not read: neither unspent nor spent.
	unknown map[string]bool
}

// unknownSpend makes the node's UTXO view of txids answer NOT_FOUND for
// every output.
func (h *harness) unknownSpend(txids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unknown == nil {
		h.unknown = map[string]bool{}
	}
	for _, id := range txids {
		h.unknown[id] = true
	}
}

var reStatus = regexp.MustCompile(`"status":"[A-Z_]*"`)

// prune makes the node answer neither the bytes nor a proof of txids.
func (h *harness) prune(txids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pruned == nil {
		h.pruned = map[string]bool{}
	}
	for _, id := range txids {
		h.pruned[id] = true
	}
}

// serveChain is the chain, with the onSend hook in front of it.
func (h *harness) serveChain(w http.ResponseWriter, r *http.Request) {
	for _, pre := range []string{"/api/v1/tx/", "/api/v1/merkle_proof/", "/api/v1/txmeta/"} {
		if id, ok := strings.CutPrefix(r.URL.Path, pre); ok {
			h.mu.Lock()
			gone := h.pruned[strings.TrimSuffix(id, "/json")]
			h.mu.Unlock()
			if gone {
				http.NotFound(w, r)
				return
			}
		}
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/utxos/"); ok {
		h.mu.Lock()
		unknown := h.unknown[strings.TrimSuffix(id, "/json")]
		h.mu.Unlock()
		if unknown {
			rec := httptest.NewRecorder()
			h.chain.ServeHTTP(rec, r)
			w.WriteHeader(rec.Code)
			_, _ = w.Write(reStatus.ReplaceAll(rec.Body.Bytes(), []byte(`"status":"NOT_FOUND"`)))
			return
		}
	}
	if r.URL.Path == "/rpc" {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if json.Unmarshal(body, &req) == nil && req.Method == "sendrawtransaction" && len(req.Params) > 0 {
			h.mu.Lock()
			hook, refuse := h.onSend, h.refuse
			h.onSend = nil
			h.mu.Unlock()
			if raw, ok := req.Params[0].(string); ok && (hook != nil || refuse != nil) {
				if tx, err := transaction.NewTransactionFromHex(raw); err == nil {
					if hook != nil {
						hook(tx)
					}
					if refuse != nil && refuse(tx) {
						http.Error(w, "refused by the test", http.StatusInternalServerError)
						return
					}
				}
			}
		}
	}
	h.chain.ServeHTTP(w, r)
}

// global is the command's state as the home name would run it, for a test
// that calls below the command line.
func (h *harness) global(name string) (*global, *bytes.Buffer, *bytes.Buffer) {
	h.t.Helper()
	env := func(k string) string {
		if k == "BORG_HOME" {
			return filepath.Join(h.dir, name)
		}
		return h.env[k]
	}
	cfg, err := config.Load(config.Path("", env), env)
	if err == nil {
		cfg, err = cfg.Resolve()
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var out, errb bytes.Buffer
	return &global{cfg: cfg, stdin: strings.NewReader(""), stdout: &out, stderr: &errb, getenv: env}, &out, &errb
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), chain: testchain.New(700)}
	h.a, h.b = testchain.NewHost(h.chain), testchain.NewHost(h.chain)
	h.chainS = httptest.NewServer(http.HandlerFunc(h.serveChain))
	h.aS, h.bS = httptest.NewServer(h.a), httptest.NewServer(h.b)
	h.plane = &testchain.Plane{Hosts: []http.Handler{h.a, h.b}}
	h.planeS = httptest.NewServer(h.plane)
	for _, s := range []*httptest.Server{h.chainS, h.aS, h.bS, h.planeS} {
		t.Cleanup(s.Close)
	}
	h.env = map[string]string{
		"BORG_NETWORK":    "regtest",
		"BORG_ASSET":      h.chainS.URL,
		"BORG_RPC":        h.chainS.URL + "/rpc",
		"BORG_SETTLE":     "rpc:" + h.chainS.URL + "/rpc",
		"BORG_FACADE":     h.planeS.URL,
		"BORG_HOSTS":      h.aS.URL + "," + h.bS.URL,
		"BORG_HEADER_URL": h.chainS.URL,
		"BORG_DELAY":      "0s",
		"BORG_TREE_COUNT": "8",
		"LANG":            "C.UTF-8",
	}
	return h
}

type result struct {
	code           int
	stdout, stderr string
}

// run runs the command as the identity whose home is name.
func (h *harness) run(name, stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	env := func(k string) string {
		if k == "BORG_HOME" {
			return filepath.Join(h.dir, name)
		}
		return h.env[k]
	}
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb, env)
	return result{code, out.String(), errb.String()}
}

// must runs a command that must exit 0.
func (h *harness) must(name string, args ...string) result {
	h.t.Helper()
	r := h.run(name, "", args...)
	if r.code != 0 {
		h.t.Fatalf("%s: borg %s: exit %d\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

func (h *harness) want(r result, code int, has string) {
	h.t.Helper()
	if r.code != code || !strings.Contains(r.stderr+r.stdout, has) {
		h.t.Fatalf("exit %d, want %d with %q\nstdout:\n%s\nstderr:\n%s", r.code, code, has, r.stdout, r.stderr)
	}
}

var (
	identityLine = regexp.MustCompile(`(?m)^identity     ([0-9a-f]{66})$`)
	hallLine     = regexp.MustCompile(`(?m)^hall  ([a-z_]+)$`)
	groupLine    = regexp.MustCompile(`(?m)^group   ([0-9a-f]{32})$`)
)

// identity initialises a home, funds it when fund is set, and returns its
// identity key.
func (h *harness) identity(name string, fund bool) string {
	h.t.Helper()
	r := h.must(name, "init")
	id := identityLine.FindStringSubmatch(r.stdout)
	if id == nil {
		h.t.Fatalf("init printed no identity:\n%s", r.stdout)
	}
	if fund {
		h.must(name, "fund", "-blocks", "102")
	}
	return id[1]
}

// newHall creates a hall, has both hosts carry it, and configures it.
func (h *harness) newHall(name string) string {
	h.t.Helper()
	r := h.must(name, "hall", "new", "example")
	m := hallLine.FindStringSubmatch(r.stdout)
	if m == nil || !strings.Contains(r.stdout, "host  BORG_HALLS="+m[1]) {
		h.t.Fatalf("hall new:\n%s", r.stdout)
	}
	h.a.Carry(m[1])
	h.b.Carry(m[1])
	h.env["BORG_HALL"] = m[1]
	h.hall = m[1]
	return m[1]
}

// startPaid serves host a's terms route, paid to the home name's identity,
// with history priced at price, and configures history_host.
func (h *harness) startPaid(t *testing.T, name string, price uint64) {
	t.Helper()
	keyFile := filepath.Join(h.dir, name+".payee")
	h.must(name, "payee", "key", "-out", keyFile)
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ec.PrivateKeyFromHex(strings.TrimSpace(strings.TrimPrefix(string(raw), "BORG_PAYEE_KEY=")))
	if err != nil {
		t.Fatal(err)
	}
	h.a.Priced = true
	h.paid = testchain.NewPaid(h.a, k, map[string]uint64{"history": price, "history-after": price})
	h.paidS = httptest.NewServer(h.paid)
	t.Cleanup(h.paidS.Close)
	h.env["BORG_HISTORY_HOST"] = h.paidS.URL
}

// org is an organization with three members in one group: the org home
// "org" and the member homes alice, bob and carol.
type org struct {
	key               string
	alice, bob, carol string
	group             string
}

func (h *harness) orgWithGroup() org {
	h.t.Helper()
	var o org
	o.key = h.identity("org", true)
	o.alice = h.identity("alice", false)
	o.bob = h.identity("bob", false)
	o.carol = h.identity("carol", false)
	h.newHall("org")
	r := h.must("org", "org", "create", "Example Organization", "-member", o.alice, "-member", o.bob, "-member", o.carol)
	if !strings.Contains(r.stdout, "seq     0 ") || strings.Count(r.stdout, " member\n") != 3 {
		h.t.Fatalf("org create:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "group", "create", "engineering", "-member", o.alice, "-member", o.bob, "-member", o.carol)
	m := groupLine.FindStringSubmatch(r.stdout)
	if m == nil {
		h.t.Fatalf("group create:\n%s\n%s", r.stdout, r.stderr)
	}
	o.group = m[1]
	h.env["BORG_ORG"] = o.key
	return o
}

// The stand-in host and its answered output, by shorter names.
type (
	testHost   = testchain.Host
	testOutput = testchain.Output
)
