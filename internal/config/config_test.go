package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/borg/internal/limits"
)

const orgKey = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func TestFileEnvironmentAndFlagsLayer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	file := "# a reader\nfacade = http://192.0.2.10:8080\nhosts = http://192.0.2.10:8080, http://192.0.2.11:8080\nnetwork = test\ntimeout = 5s\nhall = example_abcdefghij\n"
	if err := os.WriteFile(p, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"BORG_NETWORK": "regtest", "BORG_OBJECT_BOUND": "2048", "BORG_ORG": orgKey, "BORG_DELAY": "5s"}
	c, err := Load(p, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Facade != "http://192.0.2.10:8080" || len(c.Hosts) != 2 || c.Hosts[1] != "http://192.0.2.11:8080" ||
		c.Network != "regtest" || c.Timeout != 5*time.Second || c.ObjectBound != 2048 || c.Originator != "borg" ||
		c.Hall != "example_abcdefghij" || c.Org != orgKey || c.TreeCount != 32 || c.ChainTreeCount != 8 ||
		c.Delay != 5*time.Second || c.Freshness != time.Minute || c.Signers != 1 {
		t.Fatalf("%+v", c)
	}
	c, err = c.Apply(map[string]string{"network": "main"})
	if err != nil || c.Network != "main" {
		t.Fatalf("flag over environment: %v %s", err, c.Network)
	}
	if _, err := Load(filepath.Join(dir, "absent"), func(string) string { return "" }); err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
}

func TestTheGrammarRefuses(t *testing.T) {
	for _, bad := range []string{"facde = x\n", "no equals\n", "facade = a\nfacade = b\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := Parse(strings.NewReader("facde = x\n")); !errors.Is(err, ErrUnknownKey) {
		t.Error("an unknown key is not ErrUnknownKey")
	}
	for k, v := range map[string]string{"network": "mainnet", "timeout": "-1s", "object_bound": "0", "hall": "example",
		"tree_count": "1001", "chain_tree_count": "33", "entries": "17", "freshness": "601s", "delay": "11m",
		"org": strings.ToUpper(orgKey), "signers": "0", "hosts": strings.Repeat("http://h,", 17), "timeout ": "1s"} {
		if _, err := Defaults().Apply(map[string]string{k: v}); err == nil {
			t.Errorf("%s = %s applied", k, v)
		}
	}
	if !slices.IsSorted(Keys) {
		t.Error("Keys is not in lexicographic order")
	}
}

func TestPath(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if Path("/x/c", get) != "/x/c" {
		t.Error("the flag wins")
	}
	env["XDG_CONFIG_HOME"] = "/xdg"
	if Path("", get) != "/xdg/borg/config" {
		t.Error(Path("", get))
	}
	env["BORG_HOME"] = "/h"
	if Path("", get) != "/h/config" {
		t.Error(Path("", get))
	}
}

func TestModeQuorumAndEntries(t *testing.T) {
	c := Defaults()
	if c.Mode != ModePlane || c.Quorum != QuorumAll || c.EntriesFor() != limits.PlaneEntries {
		t.Fatalf("defaults: mode %q quorum %q entries %d", c.Mode, c.Quorum, c.EntriesFor())
	}
	c, err := c.Apply(map[string]string{"mode": "unicast", "quorum": "majority"})
	if err != nil || c.Mode != ModeUnicast || c.Quorum != QuorumMajority || c.EntriesFor() != limits.UnicastEntries {
		t.Fatalf("%v %+v", err, c)
	}
	if c, _ = c.Apply(map[string]string{"entries": "4"}); c.EntriesFor() != 4 {
		t.Fatalf("entries %d", c.EntriesFor())
	}
	for q, want := range map[string]int{"all": 3, "": 3, "majority": 2, "one": 1, "2": 2} {
		if n, err := Need(q, 3); err != nil || n != want {
			t.Errorf("Need(%q, 3) = %d %v, want %d", q, n, err, want)
		}
	}
	if _, err := Need("4", 3); err == nil {
		t.Error("a quorum above the host count")
	}
}

func TestIsKeyHex(t *testing.T) {
	if !IsKeyHex(orgKey) {
		t.Error("the generator point is a key")
	}
	for _, bad := range []string{"", orgKey[:64], "04" + orgKey[2:], strings.ToUpper(orgKey),
		"02" + strings.Repeat("ff", 32)} {
		if IsKeyHex(bad) {
			t.Errorf("%q is a key", bad)
		}
	}
}

// On main and test a home needs no node: the chain view, the header
// source and the settlement leg have defaults. regtest has none.
func TestResolveTheNoNodeDefaults(t *testing.T) {
	for _, n := range []string{"main", "test"} {
		c, err := Defaults().Apply(map[string]string{"network": n})
		if err == nil {
			c, err = c.Resolve()
		}
		if err != nil || c.Chain != "woc:"+n || c.HeaderURL != "woc:"+n || c.Settle != "arcade:"+n || c.CheckNetwork() != nil {
			t.Fatalf("%s: %v %+v", n, err, c)
		}
	}
	c, err := Defaults().Apply(map[string]string{"network": "regtest"})
	if c, err = c.Resolve(); err != nil || c.Chain != "" || c.HeaderURL != "" || c.Settle != "" {
		t.Fatalf("regtest: %v %+v", err, c)
	}
	// The older key asset is the chain view asset:<URL>.
	c, _ = Defaults().Apply(map[string]string{"asset": "http://192.0.2.1:8090"})
	if c, err = c.Resolve(); err != nil || c.Chain != "asset:http://192.0.2.1:8090" || c.AssetURL() != "http://192.0.2.1:8090" {
		t.Fatalf("asset: %v %+v", err, c)
	}
	c, _ = Defaults().Apply(map[string]string{"asset": "http://192.0.2.1:8090", "chain": "woc:main"})
	if _, err := c.Resolve(); err == nil {
		t.Fatal("asset and chain both set resolved")
	}
	c, _ = Defaults().Apply(map[string]string{"chain": "woc:test,spend=asset:http://192.0.2.1:8090"})
	if c.AssetURL() != "http://192.0.2.1:8090" {
		t.Fatal(c.AssetURL())
	}
	for _, bad := range []map[string]string{{"network": "main", "chain": "woc:test"}, {"network": "test", "settle": "arcade:main"}} {
		c, _ := Defaults().Apply(bad)
		if c, _ = c.Resolve(); c.CheckNetwork() == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

func TestFeeKeys(t *testing.T) {
	c, err := Defaults().Apply(map[string]string{"fee_rate": "50/1000", "fee_floor": "1", "fee_dust": "1", "fee_max_rate": "1/1",
		"fee_max_tx": "100000", "fee_min_rate": "1/1000", "fee_source": "arc", "fee_policy_urls": "https://a.example, https://b.example"})
	if err != nil {
		t.Fatal(err)
	}
	f := c.Fee
	if f.Rate.Sats != 50 || f.Rate.Bytes != 1000 || *f.Floor != 1 || *f.Dust != 1 || f.MaxRate.Sats != 1 || f.MaxTx != 100000 ||
		f.MinRate.Bytes != 1000 || f.Source != "arc" || len(f.PolicyURLs) != 2 {
		t.Fatalf("%+v", f)
	}
	for k, v := range map[string]string{"fee_rate": "0/1000", "fee_floor": "-1", "fee_source": "live", "fee_max_rate": "x", "fund_unmined": "maybe", "woc_rate": "0"} {
		if _, err := Defaults().Apply(map[string]string{k: v}); err == nil {
			t.Errorf("%s = %s applied", k, v)
		}
	}
}
