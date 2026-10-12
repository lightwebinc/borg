// Package config resolves the borg command's settings: flags, then BORG_*
// in the environment, then the config file, then the built-in default.
//
// The overlay addresses have no default: the overlay hosts, the facade and
// the host a priced question goes to. A default there would quietly send an
// organization's records, or a member's questions, to somebody else's
// server, so a command that needs one and finds none is a usage error.
//
// The chain services do, on mainnet and testnet, so that a home needs no
// node of its own (Resolve): headers and the chain view from WhatsOnChain
// (woc:main, woc:test), checked against their proof of work and the
// headers, and GorillaPool's public arcade to broadcast (arcade:main,
// arcade:test). A regtest chain has no public services and no defaults.
package config

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
)

// Config is the resolved settings.
type Config struct {
	// ArcadeKey is the bearer token for an arcade installation in Settle.
	ArcadeKey string
	// Asset is the node's asset API base URL. It is the older spelling of
	// Chain = asset:<URL>, and the node coinbase is mined through (regtest).
	Asset string
	// Chain is the chain view: where transactions, proofs and spends are
	// read (nodeapi.ParseChain): woc:main, woc:test, asset:<URL>, or a
	// list. Resolve defaults it to woc:<network> on main and test.
	Chain string
	// ChainTreeCount is how many outputs a new chain tree has.
	ChainTreeCount int
	// Delay is the propagation delay: how long after a mined object's
	// block is in the publisher's headers it is published (spec section
	// 7.2).
	Delay time.Duration
	// Entries is the most wrap entries one wrap record carries; zero is the
	// mode's default (8 on the plane, 16 in unicast).
	Entries int
	// Facade is the overlay host a publisher submits to in mode plane.
	Facade string
	// Fee is the miner fee policy (feepolicy.Config), from the fee_* keys.
	// Unset keys keep mint.DefaultFees.
	Fee feepolicy.Config
	// Freshness bounds how old a writer's evidence that a group's token is
	// the latest may be (spec section 6.5).
	Freshness time.Duration
	// FundUnmined is whether fund -beef takes a payment not yet mined
	// whose parents are proven: "accept" (the default) or "refuse".
	FundUnmined string
	// Hall is the hall identifier <name>_<suffix> a command uses when it
	// names none.
	Hall string
	// HeaderToken is the bearer token a header source is sent (a
	// block-headers-service requires one).
	HeaderToken string
	// HeaderURL is the header source every proof is checked against:
	// woc:main, woc:test, bhs:URL, arcade:URL, chaintracks:URL or a
	// bridge's /v1 base URL. Resolve defaults it to woc:<network> on main
	// and test.
	HeaderURL string
	// HistoryHost is the base URL of ls_borg on the host a priced history
	// question is asked of: the host's terms route (spec section 10.3).
	HistoryHost string
	// Home is the identity's directory: key, coin pool and state.
	Home string
	// Hosts are the overlay hosts a reader asks. In mode unicast they are
	// every host a publisher submits to; in mode plane mined objects also
	// go to each of them directly.
	Hosts []string
	// Mode is how a publisher's objects reach the hosts: "plane" or
	// "unicast".
	Mode string
	// Network is "main", "test" or "regtest".
	Network string
	// ObjectBound is the largest object the plane takes, in bytes.
	ObjectBound int
	// Org is the organization a member's command reads when it names none:
	// its identity key, 66 lowercase hex digits.
	Org string
	// Originator is the BRC-100 originator presented to the wallet.
	Originator string
	// Quorum is, in mode unicast, how many of Hosts must take a carrier for
	// it to count as published: "all", "majority", "one" or a number.
	// Tokens and sweeps always need every host.
	Quorum string
	// RPC is the node's JSON-RPC URL; RPCUser and RPCPass its basic auth.
	RPC     string
	RPCPass string
	RPCUser string
	// Settle is the settlement leg transactions are broadcast through
	// (publish.ParseSettler): arcade:main, arcade:test, arcade:<url>,
	// arc:<url>, rpc:<url> or tcp:<host:port>. Resolve defaults it to
	// arcade:<network> on main and test.
	Settle string
	// Signers is how many instances of the organization's tooling can sign
	// transitions. Above one, wraps wait one further block (spec section
	// 7.2).
	Signers int
	// Timeout bounds each request to a host, node or header source.
	Timeout time.Duration
	// TreeCount is how many outputs a new grant or record funding tree has.
	TreeCount int
	// WoCKey is a WhatsOnChain API key, and WoCRate the requests a second
	// its plan allows (zero is the free rate, 3).
	WoCKey  string
	WoCRate float64
}

// The modes and the named quorums.
const (
	ModePlane      = "plane"
	ModeUnicast    = "unicast"
	QuorumAll      = "all"
	QuorumMajority = "majority"
	QuorumOne      = "one"
)

// Need is how many of n hosts the quorum q requires. A number above n is an
// error: a quorum no host set can meet would refuse every publish.
func Need(q string, n int) (int, error) {
	if n < 1 {
		return 0, errors.New("no hosts")
	}
	switch q {
	case QuorumAll, "":
		return n, nil
	case QuorumMajority:
		return n/2 + 1, nil
	case QuorumOne:
		return 1, nil
	}
	k, err := strconv.Atoi(q)
	if err != nil || k < 1 {
		return 0, fmt.Errorf("quorum %q is not all, majority, one or a positive number", q)
	}
	if k > n {
		return 0, fmt.Errorf("quorum %d is more than the %d configured host(s)", k, n)
	}
	return k, nil
}

// EntriesFor is the wrap entries per record the mode takes when Entries is
// unset.
func (c Config) EntriesFor() int {
	if c.Entries > 0 {
		return c.Entries
	}
	if c.Mode == ModeUnicast {
		return limits.UnicastEntries
	}
	return limits.PlaneEntries
}

// DefaultHome is ~/.borg, or .borg when there is no home directory.
func DefaultHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".borg")
	}
	return ".borg"
}

// Defaults are the built-in values for the keys that have one.
func Defaults() Config {
	return Config{
		ChainTreeCount: limits.DefaultChainTreeCount,
		Delay:          limits.DefaultDelay,
		Freshness:      limits.DefaultFreshness,
		FundUnmined:    "accept",
		Home:           DefaultHome(),
		Mode:           ModePlane,
		Network:        "main",
		ObjectBound:    limits.DefaultObjectBound,
		Originator:     "borg",
		Quorum:         QuorumAll,
		RPCPass:        "bitcoin",
		RPCUser:        "bitcoin",
		Signers:        1,
		Timeout:        limits.DefaultTimeout,
		TreeCount:      limits.DefaultTreeCount,
	}
}

// Keys is the file grammar's key list, in lexicographic order. A key not in
// it is an error: a misspelt key that was silently ignored would leave a
// setting at its default with no sign that the file was ever read.
var Keys = []string{"arcade_key", "asset", "chain", "chain_tree_count", "delay", "entries", "facade", "fee_dust", "fee_floor",
	"fee_max_rate", "fee_max_tx", "fee_min_rate", "fee_policy_urls", "fee_rate", "fee_source", "freshness", "fund_unmined", "hall",
	"header_token", "header_url", "history_host", "home", "hosts", "mode", "network", "object_bound", "org", "originator", "quorum",
	"rpc", "rpc_pass", "rpc_user", "settle", "signers", "timeout", "tree_count", "woc_key", "woc_rate"}

// ErrUnknownKey reports a key the grammar does not define.
var ErrUnknownKey = errors.New("config: unknown key")

// Path is the config file location: the -config flag, else
// $BORG_HOME/config, else $XDG_CONFIG_HOME/borg/config, else
// ~/.borg/config.
func Path(flagPath string, getenv func(string) string) string {
	if flagPath != "" {
		return flagPath
	}
	if h := getenv("BORG_HOME"); h != "" {
		return filepath.Join(h, "config")
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "borg", "config")
	}
	return filepath.Join(DefaultHome(), "config")
}

// Parse reads the key = value grammar: one setting per line, '#' comments,
// blank lines ignored, a key at most once.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("config line %d: not key = value", line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !known(k) {
			return nil, fmt.Errorf("%w: line %d: %q", ErrUnknownKey, line, k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("config line %d: %q set twice", line, k)
		}
		out[k] = v
	}
	return out, sc.Err()
}

func known(k string) bool {
	for _, x := range Keys {
		if x == k {
			return true
		}
	}
	return false
}

// Load resolves the file (a missing one is fine) and the environment over
// the defaults. Flags are applied by the caller afterwards with Apply.
func Load(path string, getenv func(string) string) (Config, error) {
	c := Defaults()
	vals := map[string]string{}
	if f, err := os.Open(path); err == nil { //nolint:gosec // the operator's own path
		defer f.Close()
		vals, err = Parse(f)
		if err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	for _, k := range Keys {
		if v := getenv("BORG_" + strings.ToUpper(k)); v != "" {
			vals[k] = v
		}
	}
	return c.Apply(vals)
}

// IsKeyHex reports whether s is an identity key: 66 lowercase hex digits
// whose bytes meet the key rule.
func IsKeyHex(s string) bool {
	if len(s) != 66 || strings.ToLower(s) != s {
		return false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return false
	}
	_, err = guard.ParsePubKey(b)
	return err == nil && borgrec.CheckIdentity(b) == nil
}

func positiveInt(k, v string, lo, hi int, why string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("config: %s %q is not a number from %d to %d: %s (docs/limits.md)", k, v, lo, hi, why)
	}
	return n, nil
}

// Apply sets the given keys the same way for a file, the environment or
// flags, so all three parse alike.
func (c Config) Apply(vals map[string]string) (Config, error) {
	for _, k := range Keys {
		v, ok := vals[k]
		if !ok {
			continue
		}
		var err error
		switch k {
		case "arcade_key":
			c.ArcadeKey = v
		case "asset":
			c.Asset = v
		case "chain":
			c.Chain = v
		case "chain_tree_count":
			c.ChainTreeCount, err = positiveInt(k, v, 1, limits.MaxChainTreeCount, "a chain's first token carries its chain tree whole")
		case "delay":
			d, perr := time.ParseDuration(v)
			if perr != nil || d < 0 || d > limits.MaxDelay {
				return c, fmt.Errorf("config: delay %q is not a duration from 0s to %s (docs/limits.md)", v, limits.MaxDelay)
			}
			c.Delay = d
		case "entries":
			c.Entries, err = positiveInt(k, v, 1, borgrec.MaxEntries, "the record bound every reader accepts")
		case "facade":
			c.Facade = v
		case "fee_dust", "fee_floor", "fee_max_tx":
			n, perr := strconv.ParseUint(v, 10, 64)
			if perr != nil {
				return c, fmt.Errorf("config: %s %q is not a whole number of satoshis", k, v)
			}
			switch k {
			case "fee_dust":
				c.Fee.Dust = &n
			case "fee_floor":
				c.Fee.Floor = &n
			default:
				c.Fee.MaxTx = n
			}
		case "fee_max_rate", "fee_min_rate", "fee_rate":
			r, perr := mint.ParseRate(v)
			if perr != nil || r.Sats == 0 {
				return c, fmt.Errorf("config: %s %q is not a rate SATS/BYTES such as 100/1000 (satoshis per bytes)", k, v)
			}
			switch k {
			case "fee_max_rate":
				c.Fee.MaxRate = &r
			case "fee_min_rate":
				c.Fee.MinRate = &r
			default:
				c.Fee.Rate = &r
			}
		case "fee_policy_urls":
			c.Fee.PolicyURLs = nil
			for _, u := range strings.Split(v, ",") {
				if u = strings.TrimSpace(u); u != "" {
					c.Fee.PolicyURLs = append(c.Fee.PolicyURLs, u)
				}
			}
		case "fee_source":
			if v != feepolicy.SourceStatic && v != feepolicy.SourceARC {
				return c, fmt.Errorf("config: fee_source must be static or arc, got %q", v)
			}
			c.Fee.Source = v
		case "freshness":
			d, perr := time.ParseDuration(v)
			if perr != nil || d <= 0 || d > limits.MaxFreshness {
				return c, fmt.Errorf("config: freshness %q is not a duration above 0s and at most %s (spec section 6.5)", v, limits.MaxFreshness)
			}
			c.Freshness = d
		case "hall":
			if v != "" && borgrec.CheckHall(v) != nil {
				return c, fmt.Errorf("config: hall %q is not a hall identifier <name>_<suffix> (borg hall new prints one)", v)
			}
			c.Hall = v
		case "fund_unmined":
			if v != "accept" && v != "refuse" {
				return c, fmt.Errorf("config: fund_unmined must be accept or refuse, got %q", v)
			}
			c.FundUnmined = v
		case "header_token":
			c.HeaderToken = v
		case "header_url":
			c.HeaderURL = v
		case "history_host":
			c.HistoryHost = v
		case "home":
			c.Home = v
		case "hosts":
			c.Hosts = nil
			for _, h := range strings.Split(v, ",") {
				if h = strings.TrimSpace(h); h != "" {
					c.Hosts = append(c.Hosts, h)
				}
			}
			if len(c.Hosts) > limits.MaxHosts {
				return c, fmt.Errorf("config: hosts names %d hosts, over the limit of %d: a reader asks every host, and a unicast publisher uploads every object to each (docs/limits.md)", len(c.Hosts), limits.MaxHosts)
			}
		case "mode":
			if v != ModePlane && v != ModeUnicast {
				return c, fmt.Errorf("config: mode must be plane or unicast, got %q", v)
			}
			c.Mode = v
		case "network":
			if v != "main" && v != "test" && v != "regtest" {
				return c, fmt.Errorf("config: network must be main, test or regtest, got %q", v)
			}
			c.Network = v
		case "object_bound":
			c.ObjectBound, err = positiveInt(k, v, 1, limits.MaxObjectBound, "no plane admits a larger object (BRC-149)")
		case "org":
			if v != "" && !IsKeyHex(v) {
				return c, fmt.Errorf("config: org %q is not an identity key (66 lowercase hex digits)", v)
			}
			c.Org = v
		case "originator":
			c.Originator = v
		case "quorum":
			if _, qerr := Need(v, 1<<30); qerr != nil {
				return c, fmt.Errorf("config: %w", qerr)
			}
			c.Quorum = v
		case "rpc":
			c.RPC = v
		case "rpc_pass":
			c.RPCPass = v
		case "rpc_user":
			c.RPCUser = v
		case "settle":
			c.Settle = v
		case "signers":
			c.Signers, err = positiveInt(k, v, 1, 16, "the instances of the tooling that can sign transitions")
		case "timeout":
			d, perr := time.ParseDuration(v)
			if perr != nil || d <= 0 {
				return c, fmt.Errorf("config: timeout %q is not a positive duration", v)
			}
			if d < limits.MinTimeout || d > limits.MaxTimeout {
				return c, fmt.Errorf("config: timeout %s is outside the limits of %s to %s (docs/limits.md)", d, limits.MinTimeout, limits.MaxTimeout)
			}
			c.Timeout = d
		case "tree_count":
			c.TreeCount, err = positiveInt(k, v, 1, limits.MaxTreeCount, "every carrier's BEEF carries its whole funding tree")
		case "woc_key":
			c.WoCKey = v
		case "woc_rate":
			r, perr := strconv.ParseFloat(v, 64)
			if perr != nil || r <= 0 || r > 1000 {
				return c, fmt.Errorf("config: woc_rate %q is not a number of requests a second above 0 and at most 1000", v)
			}
			c.WoCRate = r
		}
		if err != nil {
			return c, err
		}
	}
	for k := range vals {
		if !known(k) {
			return c, fmt.Errorf("%w: %q", ErrUnknownKey, k)
		}
	}
	return c, nil
}

// Resolve fills what a network has a default for and is not configured:
// on main and test, the chain view woc:<network>, the header source
// woc:<network> and the settlement leg arcade:<network>. The older key
// asset becomes the chain view asset:<URL>; asset and chain both set is
// refused, since which one is meant cannot be told. A regtest chain has no
// public services, so nothing is filled for it.
func (c Config) Resolve() (Config, error) {
	if c.Asset != "" {
		if c.Chain != "" {
			return c, fmt.Errorf("config: chain and asset are both set; asset is the older spelling of chain = asset:%s, so keep one", c.Asset)
		}
		c.Chain = "asset:" + c.Asset
	}
	if c.Network != "main" && c.Network != "test" {
		return c, nil
	}
	if c.Chain == "" {
		c.Chain = "woc:" + c.Network
	}
	if c.HeaderURL == "" {
		c.HeaderURL = "woc:" + c.Network
	}
	if c.Settle == "" {
		s, err := publish.DefaultSettle(c.Network)
		if err != nil {
			return c, err
		}
		c.Settle = s
	}
	return c, nil
}

// AssetURL is the node's asset API base URL: asset, else the first
// asset:<URL> backend the chain view names, else "".
func (c Config) AssetURL() string {
	if c.Asset != "" {
		return c.Asset
	}
	for _, item := range strings.Split(c.Chain, ",") {
		item = strings.TrimSpace(item)
		if m, rest, ok := strings.Cut(item, "="); ok && !strings.Contains(m, ":") {
			item = rest
		}
		if u, ok := strings.CutPrefix(item, "asset:"); ok && u != "" {
			return u
		}
	}
	return ""
}

// CheckNetwork refuses a WhatsOnChain chain view or a public arcade named
// for another network than the configured one.
func (c Config) CheckNetwork() error {
	for _, item := range strings.Split(c.Chain, ",") {
		item = strings.TrimSpace(item)
		if m, rest, ok := strings.Cut(item, "="); ok && !strings.Contains(m, ":") {
			item = rest
		}
		if n, ok := strings.CutPrefix(item, "woc:"); ok && n != c.Network {
			return fmt.Errorf("config: chain %s is the %s network but network is %s", item, n, c.Network)
		}
	}
	if n, ok := strings.CutPrefix(strings.TrimSpace(c.Settle), "arcade:"); ok && (n == "main" || n == "test") && n != c.Network {
		return fmt.Errorf("config: settle %s is the %s network but network is %s", c.Settle, n, c.Network)
	}
	return nil
}
