package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	base58 "github.com/bsv-blockchain/go-sdk/compat/base58"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/config"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/internal/unicast"
	"github.com/lightwebinc/borg/reader"
)

// httpClient is the client for everything but submissions, which
// bcommon's facade makes itself.
var httpClient *http.Client

// poll paces proof waits; tests shorten it.
var poll = 2 * time.Second

// openWallet opens the home's embedded wallet.
func (g *global) openWallet() (*bwallet.Embedded, error) {
	e, err := bwallet.Open(g.cfg.Home, engine.Profile)
	if err != nil {
		return nil, fmt.Errorf("open the home %s: %w (run `borg init`)", g.cfg.Home, err)
	}
	e.Originator = g.cfg.Originator
	e.Mainnet = g.cfg.Network == "main"
	return e, nil
}

// lockHome takes the home's lock: one command that spends from the pool or
// writes the state at a time.
func (g *global) lockHome() (func(), error) {
	unlock, err := state.Lock(filepath.Join(g.cfg.Home, "lock"))
	if errors.Is(err, state.ErrLocked) {
		return nil, usage("another borg command is using the home %s (one at a time)", g.cfg.Home)
	}
	return unlock, err
}

// home is an open, locked home: its wallet and its state.
type home struct {
	e     *bwallet.Embedded
	st    *state.State
	close func()
}

// lockedWallet takes the home's lock and only then opens its wallet. The
// order matters: the wallet's coin pool is read into memory when it is
// opened and written back whole when it changes, so a pool read before the
// lock is one another command may change in between, and saving it would
// put back coins that command spent.
func (g *global) lockedWallet() (*bwallet.Embedded, func(), error) {
	if _, err := os.Stat(filepath.Join(g.cfg.Home, "identity.json")); err != nil {
		return nil, nil, fmt.Errorf("open the home %s: %w (run `borg init`)", g.cfg.Home, err)
	}
	unlock, err := g.lockHome()
	if err != nil {
		return nil, nil, err
	}
	e, err := g.openWallet()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return e, unlock, nil
}

// openHome locks the home, opens its wallet and loads its state.
func (g *global) openHome() (*home, error) {
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return nil, err
	}
	st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
	if err != nil {
		unlock()
		return nil, err
	}
	return &home{e: e, st: st, close: unlock}, nil
}

// orgKey is the home's identity private key: the organisation's, which
// signs every grant it issues (BRC-52's certifier).
func (g *global) orgKey() (*ec.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(g.cfg.Home, "identity.json"))
	if err != nil {
		return nil, fmt.Errorf("open the home %s: %w (run `borg init`)", g.cfg.Home, err)
	}
	var id struct {
		WIF string `json:"wif"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("identity.json: %w", err)
	}
	k, err := ec.PrivateKeyFromWif(id.WIF)
	if err != nil {
		return nil, fmt.Errorf("identity.json: %w", err)
	}
	return k, nil
}

// node is the configured node: its JSON-RPC and asset API.
func (g *global) node(needRPC bool) (*nodeapi.RPC, *nodeapi.Asset, error) {
	if g.cfg.Asset == "" || (needRPC && g.cfg.RPC == "") {
		if needRPC {
			return nil, nil, usage("rpc and asset must be configured (config keys rpc, asset)")
		}
		return nil, nil, usage("asset must be configured (config key asset): proofs and spends are read from the node")
	}
	var rpc *nodeapi.RPC
	if g.cfg.RPC != "" {
		rpc = &nodeapi.RPC{URL: g.cfg.RPC, User: g.cfg.RPCUser, Pass: g.cfg.RPCPass, ID: "borg"}
	}
	return rpc, &nodeapi.Asset{Base: g.cfg.Asset}, nil
}

// spendSource is the node's view of which outputs are spent, held to its
// form: a spender is a transaction id, 64 lowercase hex digits, or the
// answer is refused. What a node names is printed, compared and asked for
// again, and a node is somebody else's server.
type spendSource struct{ *nodeapi.Asset }

// Spender is the node's answer, refused unless it is a transaction id.
func (s spendSource) Spender(ctx context.Context, txid string, vout uint32) (string, error) {
	by, err := s.Asset.Spender(ctx, txid, vout)
	if err != nil {
		return "", reader.QuietSpend(err)
	}
	by = strings.ToLower(by)
	if _, herr := reader.Hash(by); by != "" && herr != nil {
		return "", fmt.Errorf("the node names a spender of %s.%d that is not a transaction id", txid, vout)
	}
	return by, nil
}

// spends is the configured spend source, or nil: the node's view of which
// outputs are spent.
func (g *global) spends() reader.Spends {
	if g.cfg.Asset == "" {
		return nil
	}
	return spendSource{&nodeapi.Asset{Base: g.cfg.Asset}}
}

// settler is the configured settlement leg.
func (g *global) settler() (publish.Settler, *publish.Arcade, error) {
	kind, addr, _ := strings.Cut(g.cfg.Settle, ":")
	switch kind {
	case "tcp":
		return &publish.TCPIngress{Addr: addr}, nil, nil
	case "rpc":
		return &publish.RPCSettler{RPC: &nodeapi.RPC{URL: addr, User: g.cfg.RPCUser, Pass: g.cfg.RPCPass, ID: "borg"}}, nil, nil
	case "arcade":
		a := &publish.Arcade{Base: addr, Key: g.cfg.ArcadeKey}
		return a, a, nil
	}
	return nil, nil, usage("settle must be tcp:<host:port>, rpc:<url> or arcade:<url> (config key settle)")
}

// reader is a reader over the configured hosts and header source.
func (g *global) reader() (*reader.Client, error) {
	hc, err := g.headerClient()
	if err != nil {
		return nil, err
	}
	if len(g.cfg.Hosts) == 0 {
		return nil, usage("no overlay host configured (config key hosts, or -hosts)")
	}
	rd := &reader.Client{Hosts: g.cfg.Hosts, Headers: hc, Timeout: g.cfg.Timeout, HTTP: httpClient}
	if g.cfg.Asset != "" {
		// A proof a host stored that a reorganisation left stale is
		// replaced by the node's current one (spec section 11.5).
		rd.Source = &nodeapi.Asset{Base: g.cfg.Asset}
	}
	return rd, nil
}

// hall is the hall a command uses: its -hall flag, else the configured one.
func (g *global) hall() (string, error) {
	h := g.cfg.Hall
	if h == "" {
		return "", usage("no hall: set hall (or -hall) to the hall identifier <name>_<suffix> (borg hall new prints one)")
	}
	if borgrec.CheckHall(h) != nil {
		return "", usage("hall %q is not a hall identifier <name>_<suffix>", termsafe.Abbrev(h))
	}
	return h, nil
}

// parseKey reads an identity key from 66 lowercase hex digits.
func parseKey(what, s string) ([]byte, *ec.PublicKey, error) {
	if !config.IsKeyHex(s) {
		return nil, nil, usage("%s %q is not an identity key (66 lowercase hex digits)", what, termsafe.Abbrev(s))
	}
	b, _ := hex.DecodeString(s)
	pub, err := guard.ParsePubKey(b)
	if err != nil {
		return nil, nil, usage("%s %q: %v", what, termsafe.Abbrev(s), err)
	}
	return b, pub, nil
}

// legs are the endpoints a publisher publishes and settles through. None
// has a default.
func (g *global) legs() (engine.Legs, error) {
	var l engine.Legs
	_, asset, err := g.node(false)
	if err != nil {
		return l, err
	}
	l.Asset = asset
	if l.Settler, l.Arcade, err = g.settler(); err != nil {
		return l, err
	}
	hc, err := g.headerClient()
	if err != nil {
		return l, err
	}
	l.Headers = hc
	// A publisher confirms by lookup a host that admitted nothing (spec
	// section 12), and a token at every host named, so it needs the hosts.
	rd, err := g.reader()
	if err != nil {
		return l, err
	}
	l.Reader = rd
	switch g.cfg.Mode {
	case config.ModeUnicast:
		if g.cfg.Facade != "" {
			return l, usage("facade is for mode plane; mode unicast submits to every host in hosts, so leave facade unset")
		}
		need, err := config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
		if err != nil {
			return l, usage("%v (config key quorum)", err)
		}
		l.Hosts = &unicast.Set{Hosts: g.cfg.Hosts, Need: need, Retries: engine.Retries, HTTP: httpClient, Note: g.say, Verbose: g.verbose}
	default:
		if g.cfg.Facade == "" {
			return l, usage("facade must be configured (config key facade): the overlay host a publisher submits to on the plane (or set mode = unicast to submit to every host in hosts)")
		}
		l.Facade = &publish.Facade{Base: g.cfg.Facade, HTTP: httpClient}
		var direct []string
		for _, h := range g.cfg.Hosts {
			if strings.TrimRight(h, "/") != strings.TrimRight(g.cfg.Facade, "/") {
				direct = append(direct, h)
			}
		}
		if len(direct) > 0 {
			l.Direct = &unicast.Set{Hosts: direct, Need: len(direct), Retries: engine.Retries, HTTP: httpClient, Note: g.say, Verbose: g.verbose}
		}
	}
	return l, nil
}

// engine opens a publisher over an open home and finishes whatever a
// previous run left.
func (g *global) engine(ctx context.Context, h *home) (*engine.Engine, error) {
	return g.engineFor(ctx, h, false)
}

// engineFor is engine; with lenient set, work a previous run left that
// cannot be finished now is reported and left for the next command, and
// the publisher is returned all the same. The kill switch runs so: an
// object that cannot reach a host, or a transition that cannot finish, must
// not keep a revocation from the chain.
func (g *global) engineFor(ctx context.Context, h *home, lenient bool) (*engine.Engine, error) {
	legs, err := g.legs()
	if err != nil {
		return nil, err
	}
	p := limits.Publisher{Plane: g.cfg.Mode == config.ModePlane, Hosts: len(g.cfg.Hosts), TreeCount: g.cfg.TreeCount,
		ChainTreeCount: g.cfg.ChainTreeCount, Entries: g.cfg.EntriesFor(), ObjectBound: g.cfg.ObjectBound}
	need, err := config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
	if err != nil {
		return nil, usage("%v (config key quorum)", err)
	}
	if g.cfg.Mode == config.ModeUnicast {
		p.Need = need
	}
	warnings, err := p.Check()
	if err != nil {
		return nil, usage("%v", err)
	}
	for _, w := range warnings {
		g.say("warning: %s", w)
	}
	o := engine.Options{TreeCount: g.cfg.TreeCount, ChainTreeCount: g.cfg.ChainTreeCount, TreeSats: limits.DefaultTreeSats,
		Ahead: limits.DefaultAhead, Fees: mint.DefaultFees, ObjectBound: g.cfg.ObjectBound, Poll: poll, Wait: 10 * time.Minute,
		Delay: g.cfg.Delay, Signers: g.cfg.Signers, Entries: g.cfg.EntriesFor(), PlaneNeed: need}
	if uint32(o.TreeCount) <= o.Ahead { //nolint:gosec // bounded by the limits
		o.Ahead = uint32(o.TreeCount / 2) //nolint:gosec // bounded by the limits
	}
	eng, err := engine.New(h.st, h.e.Signer(), h.e.Pool, legs, o)
	if err != nil {
		return nil, err
	}
	eng.Note, eng.Verbose = g.say, g.verbose
	if eng.Key, err = g.orgKey(); err != nil {
		return eng, err
	}
	err = eng.Start(ctx)
	var lostErr *engine.LostError
	if err != nil && !errors.As(err, &lostErr) {
		if !lenient || ctx.Err() != nil {
			return eng, err
		}
		g.say("work an earlier command left is not finished, and is left for the next command: %v", err)
		return eng, nil
	}
	// Where more than one instance of the tooling can sign, a command
	// first catches up with what another instance did (spec section 7.4):
	// after losing a race, and whenever the node shows one of the home's
	// latest tokens spent.
	if g.cfg.Signers > 1 && !g.explicitSync && h.st.Org != nil && (lostErr != nil || eng.Stale(ctx)) {
		rep, serr := eng.Sync(ctx, h.st.Org.Hall, engine.SyncOptions{})
		if serr != nil {
			if err != nil {
				g.say("%v", err)
			}
			return eng, syncFailed(serr)
		}
		g.reportSync(g.stderr, rep)
		if lostErr != nil {
			// The transition a previous command left was beaten to the
			// chain. This command builds on the tip just read.
			g.say("%v", lostErr)
		}
		err = eng.Owed(ctx)
	}
	return eng, err
}

// tallies prints, in mode unicast, each host's count of what it took and
// what it missed.
func (g *global) tallies(eng *engine.Engine) {
	if eng == nil || eng.Legs.Hosts == nil {
		return
	}
	for _, t := range eng.Legs.Hosts.Tallies() {
		line := fmt.Sprintf("host %s: took %d object(s), missed %d", t.Host, t.Took, t.Missed)
		if t.Missed > 0 {
			line += ": " + strings.Join(t.MissedWhat, ", ")
		}
		fmt.Fprintln(g.stdout, line)
	}
}

const initHelp = `usage: borg init [-from FILE]

Create the home (mode 0700): an identity key and an empty coin pool. Print
the identity key, which is a member's name in every grant and group, and an
organisation's name when this home runs one, and the fund address for the
configured network. A home that exists is left alone and printed.

-from FILE makes the home around an identity key that exists already:
FILE is another home's identity.json, carried here by hand. It is how an
organisation's home is made on a second machine; borg org sync then
rebuilds its state from the hosts. FILE is the organisation's key: carry it
as the secret it is, and never copy a whole home that is still in use.`

// adoptIdentity copies an identity file into a home that has none.
func adoptIdentity(home, from string) error {
	raw, err := os.ReadFile(from) //nolint:gosec // the operator's own path
	if err != nil {
		return err
	}
	var id struct {
		WIF string `json:"wif"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return usage("-from %s is not an identity file: %v", from, err)
	}
	if _, err := ec.PrivateKeyFromWif(id.WIF); err != nil {
		return usage("-from %s holds no identity key: %v", from, err)
	}
	dst := filepath.Join(home, "identity.json")
	if _, err := os.Stat(dst); err == nil {
		return usage("the home %s has an identity already; -from makes a new home", home)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return state.WriteAtomic(dst, raw, 0o600)
}

func cmdInit(_ context.Context, g *global, args []string) error {
	fs := g.flagSet("init", initHelp)
	from := fs.String("from", "", "another home's identity.json: make this home around that identity key")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("init takes no arguments")
	}
	if *from != "" {
		if err := adoptIdentity(g.cfg.Home, *from); err != nil {
			return err
		}
		fmt.Fprintln(g.stdout, "created", g.cfg.Home, "around the identity in", *from)
	}
	// The lock is taken before the wallet is made or opened: a home that
	// exists may be in use.
	if err := os.MkdirAll(g.cfg.Home, 0o700); err != nil {
		return err
	}
	unlock, err := g.lockHome()
	if err != nil {
		return err
	}
	defer unlock()
	e, err := bwallet.Create(g.cfg.Home, engine.Profile)
	if errors.Is(err, bwallet.ErrIdentityExists) {
		if e, err = g.openWallet(); err != nil {
			return err
		}
		if *from == "" {
			g.say("the home %s exists; left as it is", g.cfg.Home)
		} else if err := e.Pool.Save(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		fmt.Fprintln(g.stdout, "created", g.cfg.Home)
	}
	if err := os.Chmod(g.cfg.Home, 0o700); err != nil {
		return err
	}
	e.Mainnet = g.cfg.Network == "main"
	addr, err := e.FundAddress(e.Mainnet)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "identity     %s\nfund address %s (%s)\n", e.Signer().IdentityHex(), addr, g.cfg.Network)
	return nil
}

const fundHelp = `usage: borg fund [-blocks N] [-batch N] [-rescan]
       borg fund -txid TXID

With -txid, import a payment: read the mined transaction TXID with its
proof from the node (asset), check the proof against the header source,
and add every output it pays to the home's fund address to the pool. This
is how a home on a real network is funded: send BSV to the fund address
init printed, wait for one confirmation, and import it.

Without -txid, mine coinbase to the fund address through the node's
generatetoaddress, which only a chain you run answers. Coinbase can be
spent 100 blocks after it is mined, so the first fund mines 101 or more.
-rescan re-reads recent blocks for coinbase the pool lacks.`

// addressNetwork is the network a base58 P2PKH address is for.
func addressNetwork(addr string) (string, error) {
	b, err := base58.Decode(addr)
	if err != nil || len(b) != 25 {
		return "", fmt.Errorf("fund address %q does not decode", addr)
	}
	switch b[0] {
	case 0x00:
		return "main", nil
	case 0x6f:
		return "test", nil
	}
	return "", fmt.Errorf("fund address %q has version byte 0x%02x", addr, b[0])
}

// checkFundAddress refuses an address for another network than the
// configured one: above all a mainnet address on a test network.
func checkFundAddress(addr, network string) error {
	n, err := addressNetwork(addr)
	if err != nil {
		return err
	}
	if (n == "main") != (network == "main") {
		return usage("the fund address %s is a %s address and network is %s; refusing to mine to it", addr, n, network)
	}
	return nil
}

func cmdFund(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("fund", fundHelp)
	blocks := fs.Int("blocks", 101, "blocks to mine to the fund address")
	batch := fs.Int("batch", bwallet.DefaultFundBatch, "blocks per generatetoaddress call")
	rescan := fs.Bool("rescan", false, "re-read the last -blocks blocks for coinbase the pool lacks instead of mining")
	txid := fs.String("txid", "", "import this mined payment to the fund address instead of mining")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("fund takes no arguments")
	}
	if *txid != "" {
		if *rescan {
			return usage("-txid and -rescan are exclusive")
		}
		return g.importPayment(ctx, *txid)
	}
	if *blocks < 1 {
		return usage("-blocks must be at least 1")
	}
	if g.cfg.Network == "main" {
		return usage("fund without -txid mines coinbase, which only a chain you run does; on network main, import a payment with fund -txid")
	}
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return err
	}
	defer unlock()
	addr, err := e.FundAddress(e.Mainnet)
	if err != nil {
		return err
	}
	if err := checkFundAddress(addr, g.cfg.Network); err != nil {
		return err
	}
	rpc, asset, err := g.node(true)
	if err != nil {
		return err
	}
	tip, err := asset.BestHeader(ctx)
	if err != nil {
		return fmt.Errorf("node tip: %w", err)
	}
	fmt.Fprintf(g.stdout, "pool before: %d output(s), %d sat; node tip %d; fund address %s\n", e.Pool.Count(), e.Pool.Balance(), tip.Height, addr)
	var added int
	if *rescan {
		from := uint32(0)
		if uint32(*blocks) < tip.Height { //nolint:gosec // a flag value
			from = tip.Height - uint32(*blocks) //nolint:gosec // a flag value
		}
		added, err = bwallet.Rescan(ctx, e.Signer(), e.Pool, asset, from, tip.Height)
	} else {
		added, _, err = bwallet.FundFromCoinbase(ctx, e.Signer(), e.Pool, rpc, asset, *blocks, *batch)
	}
	if err != nil {
		return err
	}
	h := tip.Height
	if t2, err := asset.BestHeader(ctx); err == nil {
		h = t2.Height
	}
	fmt.Fprintf(g.stdout, "pool after:  %d output(s), %d sat (+%d); immature %d; node tip %d\n",
		e.Pool.Count(), e.Pool.Balance(), added, len(e.Pool.Immature(h)), h)
	return nil
}

// importPayment adds the outputs a mined payment pays to the fund address
// to the pool, once its proof holds against the header source.
func (g *global) importPayment(ctx context.Context, txid string) error {
	if h, err := chainhash.NewHashFromHex(txid); err != nil || h.String() != txid {
		return usage("-txid %q is not a transaction id (64 lowercase hex characters)", termsafe.Abbrev(txid))
	}
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return err
	}
	defer unlock()
	hc, err := g.headerClient()
	if err != nil {
		return err
	}
	_, asset, err := g.node(false)
	if err != nil {
		return usage("fund -txid reads the payment from the node: %v", err)
	}
	raw, err := asset.TxRaw(ctx, txid)
	if err != nil {
		return fmt.Errorf("payment %s: %w", txid, err)
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil || tx.TxID().String() != txid {
		return refused("payment %s: the node answered other bytes", txid)
	}
	mp, _, err := asset.Proof(ctx, txid)
	if err != nil {
		return fmt.Errorf("payment %s: its proof: %w (wait for it to mine)", txid, err)
	}
	tx.MerklePath = mp
	ok, err := mp.Verify(ctx, tx.TxID(), hc)
	if err != nil {
		return fmt.Errorf("payment %s: checking its proof against %s: %w", txid, g.cfg.HeaderURL, err)
	}
	if !ok {
		return refused("payment %s: its proof does not hold against the header source %s", txid, g.cfg.HeaderURL)
	}
	fund, err := e.Signer().FundScript()
	if err != nil {
		return err
	}
	var outs []bwallet.Output
	var sats uint64
	for i, o := range tx.Outputs {
		if o.LockingScript == nil || !bytes.Equal(*o.LockingScript, *fund) {
			continue
		}
		outs = append(outs, bwallet.Output{TxID: txid, Vout: uint32(i), Satoshis: o.Satoshis, //nolint:gosec // an output index
			LockingScript: o.LockingScript.String(), Height: mp.BlockHeight, Coinbase: tx.IsCoinbase(),
			Raw: tx.Hex(), Bump: mp.Hex()})
		sats += o.Satoshis
	}
	addr, _ := e.FundAddress(e.Mainnet)
	if len(outs) == 0 {
		return usage("payment %s pays nothing to the fund address %s", txid, addr)
	}
	added, err := e.Pool.Add(outs...)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "imported %d of %d output(s) paying %s, %d sat, mined at height %d; pool %d output(s), %d sat\n",
		added, len(outs), addr, sats, mp.BlockHeight, e.Pool.Count(), e.Pool.Balance())
	if added < len(outs) {
		g.say("note: %d output(s) were in the pool already", len(outs)-added)
	}
	return nil
}

const hallHelp = `usage: borg hall new <name>
       borg hall list

new draws the hall's 10-letter suffix uniformly at random and prints the
hall identifier <name>_<suffix>, its topic, and the line a host's
BORG_HALLS takes to carry it. <name> is 1 to 31 lowercase letters and
single underscores, starting and ending with a letter. The suffix is not a
secret and not an access control: it keeps unrelated halls off each
other's topic. Many organisations may share one hall. When a home exists,
the hall is recorded in it, and list prints the halls recorded.`

func cmdHall(_ context.Context, g *global, args []string) error {
	fs := g.flagSet("hall", hallHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 2 && pos[0] == "new":
		if borgrec.CheckHallName(pos[1]) != nil {
			return usage("hall name %q: 1 to %d lowercase letters and single underscores, starting and ending with a letter", termsafe.Abbrev(pos[1]), borgrec.MaxHallName)
		}
		hall, err := borgrec.NewHall(pos[1], rand.Reader)
		if err != nil {
			return err
		}
		if _, serr := os.Stat(filepath.Join(g.cfg.Home, "identity.json")); serr == nil {
			e, unlock, err := g.lockedWallet()
			if err != nil {
				return err
			}
			defer unlock()
			st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
			if err != nil {
				return err
			}
			st.AddHall(hall)
			if err := st.Save(); err != nil {
				return err
			}
		}
		fmt.Fprintf(g.stdout, "hall  %s\ntopic %s\nhost  BORG_HALLS=%s\n", hall, borgrec.Topic(hall), hall)
		return nil
	case len(pos) == 1 && pos[0] == "list":
		e, err := g.openWallet()
		if err != nil {
			return err
		}
		st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
		if err != nil {
			return err
		}
		for _, h := range st.Halls {
			fmt.Fprintln(g.stdout, h)
		}
		return nil
	}
	return usage("hall new <name> | hall list")
}

const doctorHelp = `usage: borg doctor

Report the home (identity, pool, funding trees, the organisation it runs,
any transition in flight, what is persisted and not yet published, and
every group's rotation debt) and whether the configured node, header
source, hosts and priced host answer. Reads only: it publishes nothing and
spends nothing.`

func cmdDoctor(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("doctor", doctorHelp)
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("doctor takes no arguments")
	}
	out := g.stdout
	fmt.Fprintf(out, "home        %s\n", g.cfg.Home)
	fmt.Fprintf(out, "network     %s\n", g.cfg.Network)
	if g.cfg.Hall != "" {
		fmt.Fprintf(out, "hall        %s\n", field(g.cfg.Hall))
	} else {
		fmt.Fprintln(out, "hall        not configured")
	}
	var tip uint32
	if g.cfg.Asset != "" {
		if h, err := (&nodeapi.Asset{Base: g.cfg.Asset}).BestHeader(ctx); err != nil {
			fmt.Fprintf(out, "node        %s: %v\n", g.cfg.Asset, err)
		} else {
			tip = h.Height
			fmt.Fprintf(out, "node        %s tip %d\n", g.cfg.Asset, h.Height)
		}
	} else {
		fmt.Fprintln(out, "node        not configured (asset): no spend source, so no writer evidence and no revocation check")
	}
	if e, err := g.openWallet(); err != nil {
		fmt.Fprintf(out, "identity    absent (%v)\n", err)
	} else {
		fmt.Fprintf(out, "identity    %s\n", e.Signer().IdentityHex())
		fmt.Fprintf(out, "pool        %d output(s), %d sat", e.Pool.Count(), e.Pool.Balance())
		if tip > 0 {
			fmt.Fprintf(out, ", %d immature", len(e.Pool.Immature(tip)))
		}
		if n := len(e.Pool.UnprovenTxids()); n > 0 {
			fmt.Fprintf(out, ", change from %d transaction(s) held until mined", n)
		}
		fmt.Fprintln(out)
		if st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex()); err != nil {
			fmt.Fprintf(out, "state       UNREADABLE: %v\n", err)
		} else {
			g.doctorState(out, st)
		}
	}
	if g.cfg.HeaderURL == "" {
		fmt.Fprintln(out, "headers     NOT CONFIGURED (nothing can be verified)")
	} else if hc, err := g.headerClient(); err == nil {
		if n, err := hc.CurrentHeight(ctx); err != nil {
			fmt.Fprintf(out, "headers     %s: %v\n", g.cfg.HeaderURL, err)
		} else {
			fmt.Fprintf(out, "headers     %s tip %d\n", g.cfg.HeaderURL, n)
		}
	}
	switch {
	case g.cfg.Mode == config.ModeUnicast:
		need, err := config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
		if err != nil {
			fmt.Fprintf(out, "mode        unicast: %v\n", err)
		} else {
			fmt.Fprintf(out, "mode        unicast: every object to each of %d host(s); a carrier published once %d take it (quorum %s), a token or sweep once all do\n", len(g.cfg.Hosts), need, g.cfg.Quorum)
		}
	case g.cfg.Facade != "":
		fmt.Fprintf(out, "mode        plane\nfacade      %s\n", g.cfg.Facade)
	default:
		fmt.Fprintln(out, "mode        plane\nfacade      not configured")
	}
	if g.cfg.Settle != "" {
		fmt.Fprintf(out, "settle      %s\n", g.cfg.Settle)
	} else {
		fmt.Fprintln(out, "settle      not configured")
	}
	for _, h := range g.cfg.Hosts {
		fmt.Fprintf(out, "host        %s %s\n", h, ping(ctx, h, g.cfg.Timeout))
	}
	if g.cfg.HistoryHost != "" {
		t, err := reader.FetchTerms(ctx, termsClient(g.cfg.Timeout), g.cfg.HistoryHost)
		switch {
		case errors.Is(err, reader.ErrNoTerms):
			fmt.Fprintf(out, "history     %s prices nothing\n", g.cfg.HistoryHost)
		case err != nil:
			fmt.Fprintf(out, "history     %s: %v\n", g.cfg.HistoryHost, firstLine(err.Error()))
		default:
			fmt.Fprintf(out, "history     %s serves terms (%d priced class(es))\n", g.cfg.HistoryHost, len(t.Classes))
		}
	}
	return nil
}

// doctorState reports the home's state.
func (g *global) doctorState(out interface{ Write([]byte) (int, error) }, st *state.State) {
	for _, kind := range state.TreeKinds {
		ts := st.Trees[kind]
		if ts == nil || ts.Tree == nil {
			fmt.Fprintf(out, "tree        %s: none yet\n", kind)
			continue
		}
		fmt.Fprintf(out, "tree        %s: %s %d of %d left\n", kind, ts.Tree.Txid, ts.Tree.Remaining(), ts.Tree.Count)
	}
	for _, p := range st.UnsettledTrees() {
		fmt.Fprintf(out, "tree        %s: %s signed for coin %s and not recorded as minted; the next command that publishes asks the node what became of it\n", field(p.Kind), p.Tree.Txid, p.Coin.Outpoint())
	}
	if o := st.Org; o != nil {
		fmt.Fprintf(out, "org         %s in %s: seq %d, %d grant(s), %d group(s)\n", field(o.Name), o.Hall, len(o.Tokens)-1, len(o.Grants), len(o.Groups))
		if p := o.Pending; p != nil {
			fmt.Fprintf(out, "transition  %s %s persisted and not yet mined; the next command finishes it\n", field(p.What), p.Txid)
		}
		now := time.Now()
		for i := range o.Groups {
			gr := &o.Groups[i]
			members, _, err := engine.Members(gr)
			if err != nil {
				fmt.Fprintf(out, "group       %s %s: %v\n", gr.ID, field(gr.Name), err)
				continue
			}
			var debt []string
			for _, m := range members {
				if currentAt(o, hex.EncodeToString(m), now) == nil {
					debt = append(debt, hex.EncodeToString(m))
				}
			}
			line := fmt.Sprintf("group       %s %s: %d member(s), %d token(s)", gr.ID, field(gr.Name), len(members), len(gr.Tokens))
			if recs, err := engine.GroupRecords(gr); err == nil && len(recs) > 0 {
				last := recs[len(recs)-1]
				if gr.RotateEvery > 0 {
					for _, r := range recs {
						if r.Seq == last.EpochSeq && now.Sub(time.Unix(int64(r.Created), 0)) > time.Duration(gr.RotateEvery)*time.Second { //nolint:gosec // a claim below 2^38
							line += fmt.Sprintf("; ROTATION DUE: epoch %d is older than its rotateEvery of %ds (borg epoch rotate %s)", last.Epoch, gr.RotateEvery, gr.ID)
						}
					}
				}
				if gr.EpochOf(uint32(last.Epoch)) == nil { //nolint:gosec // at most 2^32 - 1
					line += fmt.Sprintf("; KEY NOT HELD: epoch %d was begun by another instance (borg org sync opens it)", last.Epoch)
				}
			}
			if len(debt) > 0 {
				line += fmt.Sprintf("; ROTATION DEBT: %s (cleared by the group's next transition: borg org due -apply, or borg epoch rotate %s)", strings.Join(debt, ", "), gr.ID)
			}
			soon := 0
			for _, m := range members {
				if _, exp := latestGrant(o, hex.EncodeToString(m)); now.Before(exp) && exp.Sub(now) <= dueWindow {
					soon++
				}
			}
			if soon > 0 {
				line += fmt.Sprintf("; %d member(s) hold a grant that expires within %s (borg org due)", soon, dueWindow)
			}
			if fs, at, err := engine.Flagged(gr); err == nil && fs != "" {
				line += fmt.Sprintf("; INCONSISTENT: seq %d is %s, and the epoch's key is wrapped to nobody else until the group rotates (borg epoch rotate %s)", at, fs, gr.ID)
			}
			fmt.Fprintln(out, line)
		}
	}
	for _, p := range st.Outbox {
		fmt.Fprintf(out, "outbox      %s %s persisted and not confirmed published; the next command publishes it\n", p.Kind, p.Txid)
	}
	if sw := st.InFlight(); sw != nil {
		fmt.Fprintf(out, "sweep       %s in flight; the next command finishes it\n", sw.Txid)
	}
	for _, sw := range st.Sweeps {
		if sw.Failed != "" {
			fmt.Fprintf(out, "sweep       %s FAILED: %s; it revokes nothing\n", sw.Txid, field(sw.Failed))
		}
	}
	if len(st.Keys) > 0 {
		fmt.Fprintf(out, "keys        %d epoch key(s) opened as a member\n", len(st.Keys))
	}
	if n := len(st.Payments); n > 0 {
		fmt.Fprintf(out, "payments    the change of %d unmined payment(s) of this home's is spendable by its next payment, up to %d unmined in a row\n", n, limits.MaxUnminedPayments)
	}
}

// currentAt is the organisation's current grant to subject at now.
func currentAt(o *state.Org, subject string, now time.Time) *state.Grant {
	for i := range o.Grants {
		gr := &o.Grants[i]
		if gr.Subject != subject || !gr.Admitted || gr.Revoked || gr.Swept != "" || !borgrec.IsMembership(gr.Scope) {
			continue
		}
		if exp, err := borgrec.ParseExpiry(gr.Expiry); err == nil && now.Before(exp) {
			return gr
		}
	}
	return nil
}

// pingQuery is a question no class answers, which a live ls_borg refuses
// with an error.
var pingQuery = reader.Query{}

// ping asks a host a question it must refuse: the host is up and runs the
// service.
func ping(ctx context.Context, base string, timeout time.Duration) string {
	c := &reader.Client{Timeout: timeout, HTTP: httpClient}
	_, err := c.Ask(ctx, base, pingQuery)
	switch {
	case err == nil || errors.Is(err, reader.ErrHost):
		return "answering"
	case strings.Contains(err.Error(), "status 5"):
		return "answering with an ERROR (" + firstLine(err.Error()) + ")"
	}
	return "NOT ANSWERING: " + firstLine(err.Error())
}

func termsClient(timeout time.Duration) *http.Client {
	if httpClient != nil {
		return httpClient
	}
	return &http.Client{Timeout: timeout}
}

// firstLine is the first line of a message, at most 160 bytes, filtered
// for the terminal: a host's error text is someone else's.
func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return termsafe.Text(s)
}

// field is someone else's text that fills one field of a line this command
// prints (an organisation's or a group's name, a role, a class name):
// filtered for the terminal, and held to that line. A name is any UTF-8,
// and one with a line break in it would otherwise write lines of its own
// in a report, in this command's voice.
func field(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\v', '\f', 0x85, 0x2028, 0x2029:
			return ' '
		}
		return r
	}, s)
	return termsafe.Text(s)
}

// plainName refuses a name this tooling would write into a record that is
// not one line of plain text. The record takes any UTF-8; a name every
// reader can print as it is holds no line break and nothing a terminal
// would act on.
func plainName(flag, s string) error {
	if field(s) != s {
		return usage("%s holds a line break or a character a terminal would act on; a name is one line of plain text", flag)
	}
	return nil
}

// plain is a note or an error filtered for the terminal, whole. termsafe
// bounds a line at 512 columns, and a line that quotes a refusal with two
// transaction ids and a host's words is longer: it would lose its end,
// which is where it says what to do. So each line is filtered in pieces of
// 256 runes: a piece holds nothing a terminal acts on once filtered, and
// joining pieces adds nothing, so the whole is as safe as its parts. At
// most 200 lines are printed.
func plain(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > termsafe.MaxLines {
		lines = append(lines[:termsafe.MaxLines], "[truncated]")
	}
	for i, l := range lines {
		r := []rune(l)
		var b strings.Builder
		for len(r) > 0 {
			n := min(len(r), 256)
			b.WriteString(termsafe.Text(string(r[:n])))
			r = r[n:]
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}
