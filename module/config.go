// Package module is the borg overlay module in Go: tm_borg_<name>_<suffix>
// for each configured hall and ls_borg for all of them, written to
// bcommon's engine-neutral contract (github.com/lightwebinc/bcommon/module)
// so that any Go overlay host that carries the adapter mounts it. It is the
// twin of the TypeScript module under host/src (frozen), built from the Go
// core: the admission rules are borgrec.Admit, the folds and statuses
// borgrec's, the questions borgrec's classes, and the index the TypeScript
// module's chains, outpoint rows, bounded spend table and retention, with
// its journal byte for byte. Its lookup service is also a bcommon
// module.Replayer: a host repairs a hall it missed part of by ordered
// replay from a sibling host (spec section 11.6), never by an unordered
// synchronization of unspent outputs.
//
// Configuration is the TypeScript module's, read from the environment when
// the host calls the factory (docs/host.md): BORG_HALLS, BORG_STATE_DIR,
// BORG_HEADERS_URL (OVERLAY_CHAIN_TRACKER_URL when that is a native
// source), BORG_RETENTION_DAYS, BORG_MAX_BEEF, BORG_MAX_SWEEP_INPUTS,
// BORG_SPEND_TABLE, BORG_LISTEN, BORG_PRICES, BORG_PAYEE_KEY,
// BORG_SESSIONS, BORG_SESSION_TTL, the handshake and response budgets,
// BORG_ARCADE_URL, BORG_ASSET_URL (also the chain source a replay reads a
// chain's first parent from), BORG_CHAIN, BORG_WOC_KEY and the
// BORG_ACCEPT_* rule, and one of its own, BORG_JOURNAL_SYNC_MS (the rows
// journal's group commit window). Every mistake is an error, and the host
// refuses to start.
package module

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/borg/borgrec"
)

// The floors an operator cannot go under (docs/spec.md sections 11.1 to
// 11.3).
const (
	// FloorDays is the retention floor, from the moment the floor's clock
	// starts (spec section 11.3).
	FloorDays = 31
	// MinSpendTable is the floor of the bounded table of spends of
	// outpoints no carrier used (spec section 11.1).
	MinSpendTable = 65536
)

// The rows journal's group commit window (BORG_JOURNAL_SYNC_MS): lines are
// written at once and fsynced together this often, off the lookup
// service's lock; 0 fsyncs each line before the admission goes on, as the
// TypeScript module does. The default and the bound are a timed
// write-ahead log's usual ones.
const (
	DefaultJournalSyncMs = 100
	MaxJournalSyncMs     = 60_000
)

// Config is the module's configuration.
type Config struct {
	Halls          []string
	StateDir       string
	HeadersURL     string
	RetentionDays  int
	MaxBEEF        int
	MaxSweepInputs int
	SpendTable     int
	// JournalSync is the rows journal's group commit window; 0 syncs every
	// line.
	JournalSync time.Duration
	// Listen is the terms route's host:port; empty for none.
	Listen   string
	Prices   map[string]uint64
	PayeeKey *ec.PrivateKey
	Sessions termsroute.SessionBound
	Budget   termsroute.BudgetConfig
	Response termsroute.ResponseBudgetConfig
	Accept   termsroute.AcceptConfig
}

// Classes are the lookup classes as the route prices them.
var Classes = func() []termsroute.PricedClass {
	out := make([]termsroute.PricedClass, len(borgrec.Classes))
	for i, c := range borgrec.Classes {
		out[i] = termsroute.PricedClass{Name: c.Name, Free: c.Free}
	}
	return out
}()

// ParseHalls parses BORG_HALLS: hall identifiers, once each.
func ParseHalls(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("BORG_HALLS is required: name each hall this host carries")
	}
	var out []string
	for _, e := range strings.Split(raw, ",") {
		h := strings.TrimSpace(e)
		if borgrec.CheckHall(h) != nil {
			return nil, fmt.Errorf("BORG_HALLS: %q is not a hall identifier <name>_<suffix>", h)
		}
		for _, x := range out {
			if x == h {
				return nil, fmt.Errorf("BORG_HALLS names %s twice", h)
			}
		}
		out = append(out, h)
	}
	return out, nil
}

var (
	digits  = regexp.MustCompile(`^[0-9]+$`)
	rateRE  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	payeeRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func integer(name, raw string, def, min int) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if !digits.MatchString(v) || err != nil || n > termsroute.MaxSafe || int(n) < min {
		return 0, fmt.Errorf("%s: %q is not an integer of at least %d", name, raw, min)
	}
	return int(n), nil
}

// rate is a positive rate, decimals allowed, at most 1e6.
func rate(name, raw string, def float64) (float64, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if !rateRE.MatchString(v) || err != nil || !(f > 0) || f > 1e6 {
		return 0, fmt.Errorf("%s: %q is not a positive number of at most 1000000", name, raw)
	}
	return f, nil
}

// isNativeSource is an http or https URL: a header source answering
// /v1/root/<height> and /v1/tip.
func isNativeSource(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ParseConfig reads the whole configuration through getenv.
func ParseConfig(getenv func(string) string) (*Config, error) {
	halls, err := ParseHalls(getenv("BORG_HALLS"))
	if err != nil {
		return nil, err
	}
	c := &Config{Halls: halls, StateDir: getenv("BORG_STATE_DIR")}
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) {
		return nil, errors.New("BORG_STATE_DIR is required: an absolute directory kept for the host's life, where the outpoint rows live")
	}
	own := strings.TrimSpace(getenv("BORG_HEADERS_URL"))
	host := strings.TrimSpace(getenv("OVERLAY_CHAIN_TRACKER_URL"))
	switch {
	case own != "":
		if !isNativeSource(own) {
			return nil, fmt.Errorf("BORG_HEADERS_URL: %q is not an http or https URL", own)
		}
		c.HeadersURL = own
	case host != "" && isNativeSource(host):
		c.HeadersURL = host
	default:
		return nil, errors.New("BORG_HEADERS_URL is required: a native header source (/v1/root/<height>, /v1/tip) that token and sweep proofs are checked against; OVERLAY_CHAIN_TRACKER_URL is not one")
	}
	ints := []struct {
		dst      *int
		name     string
		def, min int
	}{
		{&c.RetentionDays, "BORG_RETENTION_DAYS", FloorDays, FloorDays},
		{&c.MaxBEEF, "BORG_MAX_BEEF", borgrec.DefaultMaxBEEF, borgrec.DefaultMaxBEEF},
		{&c.MaxSweepInputs, "BORG_MAX_SWEEP_INPUTS", borgrec.DefaultMaxSweepInputs, borgrec.DefaultMaxSweepInputs},
		{&c.SpendTable, "BORG_SPEND_TABLE", MinSpendTable, MinSpendTable},
	}
	for _, x := range ints {
		if *x.dst, err = integer(x.name, getenv(x.name), x.def, x.min); err != nil {
			return nil, err
		}
	}
	syncMs, err := integer("BORG_JOURNAL_SYNC_MS", getenv("BORG_JOURNAL_SYNC_MS"), DefaultJournalSyncMs, 0)
	if err != nil {
		return nil, err
	}
	if syncMs > MaxJournalSyncMs {
		return nil, fmt.Errorf("BORG_JOURNAL_SYNC_MS: %d is over %d", syncMs, MaxJournalSyncMs)
	}
	c.JournalSync = time.Duration(syncMs) * time.Millisecond
	if c.Prices, err = termsroute.ParsePrices(getenv("BORG_PRICES"), Classes, "BORG_PRICES"); err != nil {
		return nil, err
	}
	maxSessions, err := integer("BORG_SESSIONS", getenv("BORG_SESSIONS"), termsroute.DefaultMaxSessions, 1)
	if err != nil {
		return nil, err
	}
	ttl, err := integer("BORG_SESSION_TTL", getenv("BORG_SESSION_TTL"), termsroute.DefaultSessionTTL, 1)
	if err != nil {
		return nil, err
	}
	c.Sessions = termsroute.SessionBound{Max: maxSessions, TTL: time.Duration(ttl) * time.Second}
	c.Budget = termsroute.DefaultBudget
	if c.Budget.PerSec, err = rate("BORG_HANDSHAKES_PER_SEC", getenv("BORG_HANDSHAKES_PER_SEC"), termsroute.DefaultBudget.PerSec); err != nil {
		return nil, err
	}
	if c.Budget.Burst, err = integer("BORG_HANDSHAKE_BURST", getenv("BORG_HANDSHAKE_BURST"), termsroute.DefaultBudget.Burst, 1); err != nil {
		return nil, err
	}
	if c.Budget.PerAddressPerSec, err = rate("BORG_HANDSHAKES_PER_ADDR_PER_SEC", getenv("BORG_HANDSHAKES_PER_ADDR_PER_SEC"), termsroute.DefaultBudget.PerAddressPerSec); err != nil {
		return nil, err
	}
	if c.Budget.AddressBurst, err = integer("BORG_HANDSHAKE_ADDR_BURST", getenv("BORG_HANDSHAKE_ADDR_BURST"), termsroute.DefaultBudget.AddressBurst, 1); err != nil {
		return nil, err
	}
	c.Response = termsroute.DefaultResponseBudget
	if c.Response.PerSec, err = rate("BORG_RESPONSES_PER_SEC", getenv("BORG_RESPONSES_PER_SEC"), termsroute.DefaultResponseBudget.PerSec); err != nil {
		return nil, err
	}
	if c.Response.Burst, err = integer("BORG_RESPONSE_BURST", getenv("BORG_RESPONSE_BURST"), termsroute.DefaultResponseBudget.Burst, 1); err != nil {
		return nil, err
	}
	if c.Accept, err = termsroute.ParseAcceptConfig("BORG", getenv); err != nil {
		return nil, err
	}
	if listen := strings.TrimSpace(getenv("BORG_LISTEN")); listen != "" {
		h, p, err := net.SplitHostPort(listen)
		port, perr := strconv.Atoi(p)
		if err != nil || h == "" || perr != nil || !digits.MatchString(p) || port < 1 || port > 65535 {
			return nil, fmt.Errorf("BORG_LISTEN: %q is not host:port", listen)
		}
		c.Listen = listen
	}
	if k := strings.TrimSpace(getenv("BORG_PAYEE_KEY")); k != "" {
		if !payeeRE.MatchString(k) {
			return nil, errors.New("BORG_PAYEE_KEY is not 64 lowercase hex characters")
		}
		b, _ := hex.DecodeString(k)
		c.PayeeKey, _ = ec.PrivateKeyFromBytes(b)
		if c.PayeeKey == nil {
			return nil, errors.New("BORG_PAYEE_KEY is not a private key")
		}
	}
	charged := false
	for _, p := range c.Prices {
		charged = charged || p > 0
	}
	if charged && (c.Listen == "" || c.PayeeKey == nil) {
		return nil, errors.New("BORG_PRICES prices a class: BORG_LISTEN and BORG_PAYEE_KEY are required")
	}
	if len(c.Prices) > 0 && c.Listen == "" {
		return nil, errors.New("BORG_PRICES needs BORG_LISTEN, where the terms document is served")
	}
	return c, nil
}
