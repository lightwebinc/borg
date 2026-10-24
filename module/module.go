package module

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/replay"
	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/borg/borgrec"
)

// Name is the module's name, as a host's module list names it.
const Name = "borg"

// PruneEvery is how often retention runs; TickEvery how often the
// background tick runs (the payment watch; retention every PruneEvery).
const (
	PruneEvery = time.Hour
	TickEvery  = 30 * time.Second
)

// Build is the version the host logs; a binary sets it.
var Build = "unversioned"

// Mounted is the module for one configuration: the contract's Module, the
// lookup service, and what Start runs.
type Mounted struct {
	Module bmodule.Module
	LS     *Service
	// Front is the terms route, when configured.
	Front   *Front
	config  *Config
	host    bmodule.Host
	ledger  *termsroute.Ledger
	journal Journal
}

// Front is the terms route and where it listens.
type Front struct {
	*termsroute.Front
	Addr string
}

// chainView is the configured node view as a replay's chain source (the
// parent of a chain's first token, the chain tree, which no host keeps):
// the node's asset API (BORG_ASSET_URL), else WhatsOnChain (BORG_CHAIN),
// else none.
func chainView(a termsroute.AcceptConfig) replay.TreeSource {
	switch {
	case a.AssetURL != "":
		return &nodeapi.Asset{Base: a.AssetURL, Client: &http.Client{Timeout: 30 * time.Second}}
	case a.Chain != "":
		if woc, err := nodeapi.NewWoC(a.Chain, a.WocKey); err == nil {
			return woc
		}
	}
	return nil
}

// Mount builds the module for c. overlayTopics is OVERLAY_TOPICS, when
// known; journal replaces the state directory's, hdrs the configured
// header source and now the host's clock, for tests.
func Mount(host bmodule.Host, c *Config, overlayTopics []string, journal Journal, hdrs chaintracker.ChainTracker, now func() time.Time) (*Mounted, error) {
	names := map[string]bool{}
	for _, hall := range c.Halls {
		names[borgrec.Topic(hall)] = true
	}
	for _, t := range overlayTopics {
		if strings.HasPrefix(t, borgrec.TopicPrefix) && !names[t] {
			return nil, fmt.Errorf("OVERLAY_TOPICS names %s but BORG_HALLS does not configure it; it would be admitted by the default manager", t)
		}
	}
	m := host.Metrics()
	for _, kind := range AdmitKinds {
		m.Preset("borg_admitted_total", map[string]string{"kind": kind})
	}
	for _, kind := range AdmitKinds {
		m.Preset("borg_admitted_repeats_total", map[string]string{"kind": kind})
	}
	for _, reason := range Reasons {
		m.Preset("borg_refused_total", map[string]string{"reason": reason})
	}
	for _, status := range TokenStatuses {
		m.Preset("borg_token_status_total", map[string]string{"status": status})
	}
	for _, why := range []string{"evidence", "retention", "table"} {
		m.Preset("borg_dropped_total", map[string]string{"why": why})
	}
	for _, why := range []string{"cap", "idle"} {
		m.Preset("borg_sessions_evicted_total", map[string]string{"why": why})
	}
	m.Preset("borg_retractions_total", nil)
	m.Preset("borg_journal_lines_total", nil)
	m.Preset("borg_journal_wait_seconds_total", nil)
	for _, result := range []string{"ok", "error"} {
		m.Preset("borg_journal_syncs_total", map[string]string{"result": result})
	}
	m.Preset("borg_journal_sync_seconds_total", nil)
	priced := map[string]bool{}
	for name, p := range c.Prices {
		if p > 0 {
			priced[name] = true
		}
	}
	if now == nil {
		now = time.Now
	}
	supplied := hdrs != nil
	if hdrs == nil {
		hdrs = headers.New(c.HeadersURL)
	}
	if journal == nil {
		fj, err := NewFileJournal(c.StateDir)
		if err != nil {
			return nil, err
		}
		fj.SyncEvery = c.JournalSync
		fj.OnSync = journalSyncs(host)
		journal = fj
	}
	ls, err := NewService(Options{Halls: c.Halls, Host: host, Journal: journal, Priced: priced, RetentionDays: c.RetentionDays, SpendTable: c.SpendTable,
		Now: now, Trees: chainView(c.Accept), Headers: hdrs, MaxBEEF: c.MaxBEEF, MaxSweepInputs: c.MaxSweepInputs})
	if err != nil {
		return nil, err
	}
	m.Gauge("borg_halls", func() float64 { return float64(len(c.Halls)) })
	m.Gauge("borg_tokens", func() float64 { return float64(ls.Counts().Tokens) })
	m.Gauge("borg_tokens_pending", func() float64 { return float64(ls.Counts().PendingTokens) })
	m.Gauge("borg_grants", func() float64 { return float64(ls.Counts().Grants) })
	m.Gauge("borg_policies", func() float64 { return float64(ls.Counts().Policies) })
	m.Gauge("borg_wraps", func() float64 { return float64(ls.Counts().Wraps) })
	m.Gauge("borg_sweeps", func() float64 { return float64(ls.Counts().Sweeps) })
	m.Gauge("borg_outpoint_rows", func() float64 { return float64(ls.Counts().Rows) })
	m.Gauge("borg_carrier_lines", func() float64 { return float64(ls.Counts().Lines) })
	m.Gauge("borg_carrier_lines_forgotten", func() float64 { return float64(ls.Counts().Forgotten) })
	m.Gauge("borg_spend_table", func() float64 { return float64(ls.Counts().Table) })
	topics := map[string]bmodule.TopicManager{}
	for _, hall := range c.Halls {
		tm, err := NewTopicManager(borgrec.Topic(hall), host, hdrs, c.MaxBEEF, c.MaxSweepInputs)
		if err != nil {
			return nil, err
		}
		topics[borgrec.Topic(hall)] = tm
	}
	out := &Mounted{Module: bmodule.Module{Topics: topics, Lookups: map[string]bmodule.LookupService{borgrec.LookupService: ls}}, LS: ls, config: c, host: host, journal: journal}
	if c.Listen != "" {
		var receiver termsroute.Receiver
		var gate *termsroute.Gate
		if len(priced) > 0 {
			ledger, err := termsroute.OpenLedger(c.StateDir, termsroute.LayoutSpread, now)
			if err != nil {
				return nil, err
			}
			out.ledger = ledger
			receiver = ledger
			gate = termsroute.GateFor("borg", host, hdrs, c.Accept, c.StateDir)
		}
		f, err := termsroute.New(termsroute.Options{App: "borg", Service: borgrec.LookupService, Classes: Classes, PaymentProtocol: bwallet.PaymentProtocol,
			LS: RouteAnswerer{LS: ls}, Host: host, Prices: c.Prices, Payee: c.PayeeKey, Receiver: receiver, Headers: hdrs, Gate: gate,
			Sessions: c.Sessions, Budget: c.Budget, Responses: c.Response, Now: now})
		if err != nil {
			return nil, err
		}
		if out.ledger != nil {
			f.RestoreFast(out.ledger.Fast)
		}
		out.Front = &Front{Front: f, Addr: c.Listen}
	}
	terms, sessions, handshakes, responses, payments := "none", "none", "none", "none", "none"
	if c.Listen != "" {
		terms = c.Listen
		sessions = fmt.Sprintf("%d, idle %ds", c.Sessions.Max, int(c.Sessions.TTL/time.Second))
		handshakes = fmt.Sprintf("%s/s burst %d, per address %s/s burst %d", num(c.Budget.PerSec), c.Budget.Burst, num(c.Budget.PerAddressPerSec), c.Budget.AddressBurst)
		responses = fmt.Sprintf("%s/s burst %d a session", num(c.Response.PerSec), c.Response.Burst)
	}
	if out.Front != nil && out.Front.Gate != nil {
		if out.Front.Gate.Networked() {
			payments = fmt.Sprintf("fast to %d sat, payer %d, total %d", c.Accept.Policy.ThresholdSats, c.Accept.Policy.PayerLimit, c.Accept.Policy.TotalLimit)
		} else {
			payments = "held until mined (no arcade or asset URL)"
		}
	}
	pricedNames := make([]string, 0, len(priced))
	for n := range priced {
		pricedNames = append(pricedNames, n)
	}
	topicNames := make([]string, 0, len(c.Halls))
	for _, hall := range c.Halls {
		topicNames = append(topicNames, borgrec.Topic(hall))
	}
	headersLine := c.HeadersURL
	if supplied {
		headersLine = "supplied"
	}
	journalLine := "each line"
	if c.JournalSync > 0 {
		journalLine = fmt.Sprintf("every %s", c.JournalSync)
	}
	replayLine := "no chain source (a missing first token is a gap)"
	if ls.trees != nil {
		replayLine = "with a chain source"
	}
	host.Log("borg module mounted", map[string]any{"topics": strings.Join(topicNames, ","), "lookup": borgrec.LookupService, "headers": headersLine,
		"terms": terms, "priced": orNone(sortedStrings(pricedNames)), "sessions": sessions, "handshakes": handshakes, "responses": responses,
		"payments": payments, "journal_sync": journalLine, "replay": replayLine, "build": Build})
	return out, nil
}

// num writes a rate as JavaScript writes a number: 4, 0.5, 2.5.
func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func orNone(a []string) string {
	if len(a) == 0 {
		return "none"
	}
	return strings.Join(a, ",")
}

func sortedStrings(a []string) []string {
	out := append([]string(nil), a...)
	sort.Strings(out)
	return out
}

// journalSyncs counts the rows journal's timed fsyncs and logs the first
// failure of a run of them and the recovery after it.
func journalSyncs(host bmodule.Host) func(lines int, took time.Duration, err error) {
	var mu sync.Mutex
	failing := false
	return func(lines int, took time.Duration, err error) {
		m := host.Metrics()
		m.Inc("borg_journal_sync_seconds_total", nil, took.Seconds())
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.Inc("borg_journal_syncs_total", map[string]string{"result": result}, 1)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil && !failing:
			failing = true
			host.Log("ls_borg could not sync the journal; its lines stay pending and the sync is tried again", map[string]any{"lines": lines, "err": err.Error()})
		case err == nil && failing:
			failing = false
			host.Log("ls_borg journal synced again", map[string]any{"lines": lines})
		}
	}
}

// Close flushes the rows journal and closes it (Start does when its context
// ends). A host that stops without it leaves at most the last sync window's
// lines in the page cache: written, not yet on disk.
func (m *Mounted) Close() error {
	if c, ok := m.journal.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// Start serves the terms route (when configured) until ctx ends, and runs
// the background tick: the payment watch every TickEvery and retention
// every PruneEvery once the index is restored. It returns when ctx ends,
// after closing the rows journal (Close).
func (m *Mounted) Start(ctx context.Context) error {
	defer func() { _ = m.Close() }()
	go func() {
		pruned := time.Now()
		t := time.NewTicker(TickEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if m.Front != nil {
				m.Front.Tick(ctx)
			}
			if !m.LS.Restored() || time.Since(pruned) < PruneEvery {
				continue
			}
			pruned = time.Now()
			m.LS.Prune(ctx)
		}
	}()
	if m.Front == nil {
		<-ctx.Done()
		return nil
	}
	return m.Front.Front.Listen(ctx, m.Front.Addr)
}

// New is the factory a host calls: the configuration from getenv, checked
// against OVERLAY_TOPICS.
func New(host bmodule.Host, getenv func(string) string) (*Mounted, error) {
	if getenv == nil {
		return nil, errors.New("borg: no environment")
	}
	var topics []string
	for _, t := range strings.Split(getenv("OVERLAY_TOPICS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			topics = append(topics, t)
		}
	}
	c, err := ParseConfig(getenv)
	if err != nil {
		return nil, err
	}
	return Mount(host, c, topics, nil, nil, nil)
}
