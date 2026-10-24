package module

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/termsroute"
)

// The module as a Go host mounts it (module.test.ts): its configuration
// read from the environment and refused rather than guessed, and the
// factory's result held to the shape a host mounts.

const (
	cfgHall  = "example_hall_vkqzmtrwxp"
	cfgOther = "support_mkvbqzxtrw"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseHalls(t *testing.T) {
	got, err := ParseHalls(" " + cfgHall + "," + cfgOther + " ")
	if err != nil || strings.Join(got, ",") != cfgHall+","+cfgOther {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "example_hall", cfgHall + "," + cfgHall, "Example_hall_vkqzmtrwxp", cfgHall + ",", "a__b_abcdefghij"} {
		if _, err := ParseHalls(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestParseConfig(t *testing.T) {
	base := map[string]string{"BORG_HALLS": cfgHall, "BORG_STATE_DIR": "/var/lib/borg", "BORG_HEADERS_URL": "http://127.0.0.1:1"}
	plus := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := ParseConfig(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.RetentionDays != 31 || c.MaxBEEF != 262144 || c.MaxSweepInputs != 1024 || c.SpendTable != 65536 || len(c.Prices) != 0 || c.Listen != "" ||
		c.HeadersURL != "http://127.0.0.1:1" || c.JournalSync != 100*time.Millisecond {
		t.Fatalf("defaults %+v", c)
	}
	fails := func(m map[string]string, want string) {
		t.Helper()
		if _, err := ParseConfig(env(m)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v: %v, want %q", m, err, want)
		}
	}
	fails(map[string]string{"BORG_HALLS": cfgHall, "BORG_HEADERS_URL": "http://127.0.0.1:1"}, "BORG_STATE_DIR")
	fails(plus("BORG_STATE_DIR", "relative"), "BORG_STATE_DIR")
	// The header source: the module's own, or the host's when it is a
	// native URL.
	if c, err := ParseConfig(env(map[string]string{"BORG_HALLS": cfgHall, "BORG_STATE_DIR": "/x", "OVERLAY_CHAIN_TRACKER_URL": "https://headers.example"})); err != nil || c.HeadersURL != "https://headers.example" {
		t.Fatalf("the host's header source: %v %v", c, err)
	}
	fails(map[string]string{"BORG_HALLS": cfgHall, "BORG_STATE_DIR": "/x", "OVERLAY_CHAIN_TRACKER_URL": "woc:main"}, "BORG_HEADERS_URL is required")
	fails(map[string]string{"BORG_HALLS": cfgHall, "BORG_STATE_DIR": "/x"}, "BORG_HEADERS_URL is required")
	fails(plus("BORG_HEADERS_URL", "ftp://x"), "BORG_HEADERS_URL")
	fails(plus("BORG_RETENTION_DAYS", "30"), "at least 31")
	fails(plus("BORG_MAX_BEEF", "1000"), "at least 262144")
	fails(plus("BORG_MAX_SWEEP_INPUTS", "1023"), "at least 1024")
	fails(plus("BORG_SPEND_TABLE", "100"), "at least 65536")
	fails(plus("BORG_LISTEN", "nowhere"), "host:port")
	fails(plus("BORG_PAYEE_KEY", strings.Repeat("AB", 32)), "64 lowercase hex")
	fails(plus("BORG_PRICES", "history=5,history-after=5"), "BORG_LISTEN")
	fails(plus("BORG_PRICES", "history=5", "BORG_LISTEN", "127.0.0.1:1", "BORG_PAYEE_KEY", strings.Repeat("11", 32)), "together")
	fails(plus("BORG_PRICES", "history=0"), "BORG_LISTEN")
	fails(plus("BORG_PRICES", "org=1", "BORG_LISTEN", "127.0.0.1:1"), "free")
	fails(plus("BORG_JOURNAL_SYNC_MS", "60001"), "BORG_JOURNAL_SYNC_MS")
	fails(plus("BORG_JOURNAL_SYNC_MS", "-1"), "BORG_JOURNAL_SYNC_MS")
	priced, err := ParseConfig(env(plus("BORG_PRICES", "history=5,history-after=5", "BORG_LISTEN", "[::1]:8081", "BORG_PAYEE_KEY", strings.Repeat("11", 32))))
	if err != nil || priced.Listen != "[::1]:8081" || priced.Prices["history"] != 5 || priced.PayeeKey == nil {
		t.Fatalf("priced %+v %v", priced, err)
	}
	if c, err := ParseConfig(env(plus("BORG_RETENTION_DAYS", "90", "BORG_MAX_BEEF", "1048576", "BORG_SPEND_TABLE", "100000", "BORG_JOURNAL_SYNC_MS", "0"))); err != nil ||
		c.SpendTable != 100000 || c.RetentionDays != 90 || c.JournalSync != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	if c.Response != termsroute.DefaultResponseBudget {
		t.Fatalf("the budget of signed responses a session has a default: %+v", c.Response)
	}
	if c, err := ParseConfig(env(plus("BORG_RESPONSES_PER_SEC", "0.5", "BORG_RESPONSE_BURST", "3"))); err != nil || c.Response.PerSec != 0.5 || c.Response.Burst != 3 {
		t.Fatalf("%+v %v", c, err)
	}
	fails(plus("BORG_RESPONSE_BURST", "0"), "BORG_RESPONSE_BURST")
	fails(plus("BORG_RESPONSES_PER_SEC", "0"), "BORG_RESPONSES_PER_SEC")
	if c.Sessions.Max != termsroute.DefaultMaxSessions || c.Sessions.TTL != time.Duration(termsroute.DefaultSessionTTL)*time.Second {
		t.Fatalf("the BRC-104 session bound has a default: %+v", c.Sessions)
	}
	if c, err := ParseConfig(env(plus("BORG_SESSIONS", "50", "BORG_SESSION_TTL", "60"))); err != nil || c.Sessions.Max != 50 || c.Sessions.TTL != time.Minute {
		t.Fatalf("%+v %v", c, err)
	}
	fails(plus("BORG_SESSIONS", "0"), "BORG_SESSIONS")
	if c.Budget != termsroute.DefaultBudget {
		t.Fatalf("the handshake budget has a default: %+v", c.Budget)
	}
	if c, err := ParseConfig(env(plus("BORG_HANDSHAKES_PER_SEC", "2.5", "BORG_HANDSHAKE_BURST", "5", "BORG_HANDSHAKES_PER_ADDR_PER_SEC", "0.5", "BORG_HANDSHAKE_ADDR_BURST", "2"))); err != nil ||
		c.Budget.PerSec != 2.5 || c.Budget.Burst != 5 || c.Budget.PerAddressPerSec != 0.5 || c.Budget.AddressBurst != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, kv := range [][2]string{{"BORG_HANDSHAKES_PER_SEC", "0"}, {"BORG_HANDSHAKES_PER_ADDR_PER_SEC", "fast"}, {"BORG_HANDSHAKE_BURST", "0"}, {"BORG_HANDSHAKE_ADDR_BURST", "1.5"}} {
		fails(plus(kv[0], kv[1]), kv[0])
	}
}

func TestFactoryMountsEachHallAndTheLookupService(t *testing.T) {
	dir := t.TempDir()
	m := map[string]string{"BORG_HALLS": cfgHall + "," + cfgOther, "BORG_STATE_DIR": dir, "BORG_HEADERS_URL": "http://127.0.0.1:1",
		"OVERLAY_TOPICS": "tm_anytx,tm_borg_" + cfgHall + ",tm_borg_" + cfgOther}
	host := newFakeHost()
	mounted, err := New(host, env(m))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mounted.Close() }()
	var topics []string
	for name, tm := range mounted.Module.Topics {
		if tm == nil {
			t.Fatal("a nil manager")
		}
		topics = append(topics, name)
	}
	sort.Strings(topics)
	if strings.Join(topics, ",") != "tm_borg_"+cfgHall+",tm_borg_"+cfgOther {
		t.Fatalf("topics %v", topics)
	}
	if len(mounted.Module.Lookups) != 1 || mounted.Module.Lookups["ls_borg"] == nil {
		t.Fatalf("lookups %v", mounted.Module.Lookups)
	}
	if !host.has("borg_refused_total", map[string]string{"reason": "beef"}) || !host.has("borg_token_status_total", map[string]string{"status": "rejoin-unrotated"}) {
		t.Fatal("counters are preset")
	}
	if host.gauge("borg_outpoint_rows") < 0 || host.gauge("borg_spend_table") < 0 {
		t.Fatal("gauges are registered")
	}
	if host.logged("borg module mounted") != 1 {
		t.Fatal("the mount is logged")
	}
	// A tm_borg_ topic the host carries but the module does not configure
	// would fall to another manager: refused.
	m["OVERLAY_TOPICS"] = "tm_borg_" + cfgHall + ",tm_borg_third_mkvbqzxtrw"
	if _, err := New(newFakeHost(), env(m)); err == nil || !strings.Contains(err.Error(), "tm_borg_third_mkvbqzxtrw") {
		t.Fatalf("an unconfigured hall: %v", err)
	}
	delete(m, "BORG_HALLS")
	if _, err := New(newFakeHost(), env(m)); err == nil || !strings.Contains(err.Error(), "BORG_HALLS") {
		t.Fatalf("no halls: %v", err)
	}
	if _, err := New(newFakeHost(), nil); err == nil {
		t.Fatal("no environment")
	}
}
