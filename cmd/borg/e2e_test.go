//go:build e2e

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lightwebinc/borg/internal/testchain"
)

// The client end to end: the borg command against two local reference
// overlay hosts running the borg host module, over a local chain (package
// testchain: the node's RPC and asset API, and the header source the hosts
// and the command check proofs against). Nothing leaves the machine.
//
//	make e2e-client REFERENCE_HOST=/path/to/reference-host NODE=/path/to/node24
//
// It needs Docker for the hosts' MySQL.

const e2ePassword = "borg-e2e"

type refHost struct {
	t     *testing.T
	dir   string
	node  string
	name  string
	env   []string
	proc  *exec.Cmd
	out   *bytes.Buffer
	url   string
	terms string
	state string
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func mysql(t *testing.T) int {
	t.Helper()
	id := docker(t, "run", "-d", "--rm", "-e", "MYSQL_ROOT_PASSWORD="+e2ePassword, "-e", "MYSQL_DATABASE=overlay_a",
		"-p", "127.0.0.1::3306", "--tmpfs", "/var/lib/mysql", "mysql:9")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port := docker(t, "port", id, "3306/tcp")
	port = strings.Split(port, "\n")[0]
	p, _ := strconv.Atoi(port[strings.LastIndex(port, ":")+1:])
	for i := 0; i < 120; i++ {
		if exec.Command("docker", "exec", id, "mysql", "-h127.0.0.1", "-uroot", "-p"+e2ePassword, "-e", "create database if not exists overlay_b").Run() == nil {
			return p
		}
		time.Sleep(time.Second)
	}
	t.Fatal("MySQL did not come up")
	return 0
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (h *refHost) start() {
	h.t.Helper()
	h.out = &bytes.Buffer{}
	h.proc = exec.Command(h.node, filepath.Join(h.dir, "dist", "index.js"))
	h.proc.Env = h.env
	h.proc.Stdout, h.proc.Stderr = h.out, h.out
	if err := h.proc.Start(); err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		if resp, err := http.Get(h.url + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	h.stop()
	h.t.Fatalf("host %s did not become ready:\n%s", h.name, h.out)
}

func (h *refHost) stop() {
	if h.proc == nil || h.proc.Process == nil {
		return
	}
	_ = h.proc.Process.Signal(syscall.SIGTERM)
	_ = h.proc.Wait()
	h.proc = nil
}

// refHosts stages the bundle as a deployment places it (in a modules/
// directory whose only node_modules is the reference host's), starts one
// MySQL and a reference host per name, each carrying hall with its terms
// route pricing history, paid to payeeKey, and returns them by name.
func refHosts(t *testing.T, chainURL, hall, payeeKey string, names ...string) map[string]*refHost {
	t.Helper()
	dir := os.Getenv("BORG_E2E_REFERENCE_HOST")
	node := os.Getenv("BORG_E2E_NODE")
	if node == "" {
		node = "node"
	}
	raw, err := os.ReadFile("../../host/bundle/borg-module.js")
	if err != nil {
		t.Fatalf("the host module bundle is not built: %v", err)
	}
	staging := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "node_modules"), filepath.Join(staging, "node_modules")); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(staging, "modules", "borg", "borg-module.js")
	if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	db := mysql(t)
	hosts := map[string]*refHost{}
	for _, name := range names {
		port, terms := freePort(t), freePort(t)
		st := filepath.Join(staging, "state-"+name)
		rh := &refHost{t: t, dir: dir, node: node, name: name, url: fmt.Sprintf("http://127.0.0.1:%d", port),
			terms: fmt.Sprintf("http://127.0.0.1:%d", terms), state: st}
		rh.env = []string{
			"PATH=" + os.Getenv("PATH"),
			"OVERLAY_TOPICS=tm_borg_" + hall,
			"OVERLAY_MODULES=" + module,
			fmt.Sprintf("OVERLAY_KNEX_URL=mysql://root:%s@127.0.0.1:%d/overlay_%s", e2ePassword, db, name),
			"OVERLAY_CHAIN_TRACKER_URL=" + chainURL,
			"OVERLAY_ADMIN_TOKEN=borg-e2e",
			"OVERLAY_LISTEN=127.0.0.1",
			"OVERLAY_PORT=" + strconv.Itoa(port),
			"BORG_HALLS=" + hall,
			"BORG_STATE_DIR=" + st,
			fmt.Sprintf("BORG_LISTEN=127.0.0.1:%d", terms),
			"BORG_PRICES=history=5,history-after=5",
			"BORG_PAYEE_KEY=" + payeeKey,
			"BORG_SESSIONS=64",
		}
		rh.start()
		t.Cleanup(rh.stop)
		hosts[name] = rh
	}
	return hosts
}

// proxy is a reverse proxy to a host, for the plane stand-in.
func proxy(t *testing.T, base string) http.Handler {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

var (
	paidLine  = regexp.MustCompile(`(?m)^paid 5 sat to \S+ in ([0-9a-f]{64})`)
	rootLine  = regexp.MustCompile(`(?m)^host (\S+): (\d+) grant\(s\), the root of seq (\d+) reproduced$`)
	hostsLine = regexp.MustCompile(`(?m)^host (\S+): .*$`)
)

func TestClientE2E(t *testing.T) {
	if os.Getenv("BORG_E2E_REFERENCE_HOST") == "" {
		t.Skip("set BORG_E2E_REFERENCE_HOST (make e2e-client REFERENCE_HOST=...)")
	}
	h := newHarness(t)
	n := 0
	step := func(what string) { n++; t.Logf("ok %2d  %s", n, what) }
	o := org{key: h.identity("org", true), alice: h.identity("alice", false), bob: h.identity("bob", true), carol: h.identity("carol", false)}
	h.identity("payee", false)
	hall := h.newHall("org")
	keyFile := filepath.Join(h.dir, "payee.env")
	h.must("payee", "payee", "key", "-out", keyFile)
	rawKey, _ := os.ReadFile(keyFile)
	payeeKey := strings.TrimSpace(strings.TrimPrefix(string(rawKey), "BORG_PAYEE_KEY="))
	step("init and fund the homes (an organisation, three members, a host's payee); hall new " + hall)

	hosts := refHosts(t, h.chainS.URL, hall, payeeKey, "a", "b")
	a, b := hosts["a"], hosts["b"]
	if !strings.Contains(a.out.String(), "borg module mounted") {
		t.Fatalf("host a did not log the module's mount line:\n%s", a.out)
	}
	plane := httptest.NewServer(&testchain.Plane{Hosts: []http.Handler{proxy(t, a.url), proxy(t, b.url)}})
	t.Cleanup(plane.Close)
	h.env["BORG_FACADE"] = plane.URL
	h.env["BORG_HOSTS"] = a.url + "," + b.url
	h.env["BORG_HISTORY_HOST"] = a.terms
	step("two reference hosts loading the bundle, each carrying the hall, each with a terms route pricing history")

	// Plane: every object submitted once to the facade, which the stand-in
	// plane delivers to both hosts; tokens and sweeps also to each host.
	r := h.must("org", "org", "create", "Example Organisation", "-member", o.alice, "-member", o.bob, "-member", o.carol)
	if strings.Count(r.stdout, "grant   ") != 3 {
		t.Fatalf("org create:\n%s\n%s", r.stdout, r.stderr)
	}
	h.env["BORG_ORG"] = o.key
	step("plane: org create, three grants published, the organisation's first token admits them")
	r = h.must("org", "group", "create", "engineering", "-member", o.alice, "-member", o.bob, "-member", o.carol)
	o.group = groupLine.FindStringSubmatch(r.stdout)[1]
	step("plane: group create with the three members, epoch 0 wrapped to each")
	for _, m := range []string{"alice", "bob", "carol"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   0 OPENED") {
			t.Fatalf("%s open:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
	step("every member opens epoch 0 from the hosts' answers")
	r = h.must("org", "org", "remove", o.bob, "-group", o.group)
	if !strings.Contains(r.stdout, "epoch 1 (a new epoch") {
		t.Fatalf("remove:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stdout, "epoch 2 (a new epoch") {
		t.Fatalf("rotate:\n%s\n%s", r.stdout, r.stderr)
	}
	step("remove bob: epoch 1; then a scheduled rotation: epoch 2")
	for _, m := range []string{"alice", "carol"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   1 OPENED") || !strings.Contains(r.stdout, "epoch   2 OPENED") || strings.Contains(r.stderr, "DISAGREES") {
			t.Fatalf("%s open:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
	r = h.run("bob", "", "open", o.group)
	h.want(r, exitRefused, "removed at seq 1")
	if !strings.Contains(r.stdout, "epoch   1 CANNOT OPEN") || !strings.Contains(r.stdout, "epoch   2 CANNOT OPEN") {
		t.Fatalf("bob:\n%s\n%s", r.stdout, r.stderr)
	}
	step("alice and carol open epochs 1 and 2; bob opens neither (exit 1), and says why")
	r = h.must("org", "grant", "revoke", o.carol, "-by", "sweep")
	if !strings.Contains(r.stdout, "sweep   ") {
		t.Fatalf("revoke by sweep:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("carol", "", "member", o.carol), exitRefused, "revoked: its revocation outpoint is spent by")
	h.must("alice", "member", o.alice)
	step("revoke carol's grant by sweep: both hosts admit the sweep; carol is no longer a current member, alice is")
	r = h.must("alice", "roll")
	if got := rootLine.FindAllStringSubmatch(r.stderr, -1); len(got) != 2 {
		t.Fatalf("roll at both hosts:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "member-of", o.alice)
	if !strings.Contains(r.stdout, o.group) {
		t.Fatalf("member-of:\n%s\n%s", r.stdout, r.stderr)
	}
	step("both hosts agree: the roll's root reproduced at each; member-of; no host disagrees on the group chain")

	r = h.must("bob", "terms")
	if !strings.Contains(r.stdout, "history        5 sat a question") {
		t.Fatalf("terms:\n%s", r.stdout)
	}
	r = h.must("bob", "history", "-chain", o.group)
	pm := paidLine.FindStringSubmatch(r.stdout)
	if pm == nil || strings.Count(r.stdout, "\nseq ") < 2 {
		t.Fatalf("history:\n%s\n%s", r.stdout, r.stderr)
	}
	step("terms, then history: the host answers 402, the command pays it (BRC-104, BRC-105, AuthFetch) and reads the group's chain")
	r = h.must("payee", "payee", "settle", filepath.Join(a.state, "payments.jsonl"))
	if !strings.Contains(r.stdout, "1 payment(s) settled, 5 sat") || !h.chain.Mined(pm[1]) {
		t.Fatalf("payee settle:\n%s\n%s", r.stdout, r.stderr)
	}
	step("payee settle: the host's recorded payment broadcast in parallel, mined and pooled")

	// One coin, three priced questions inside one block: each payment
	// after the first spends the unmined change of the one before, and the
	// host verifies it through that payment.
	h.identity("erin", false)
	h.must("erin", "fund", "-blocks", "101")
	h.chain.SetHold(true)
	var paid []string
	for range 3 {
		r := h.must("erin", "history", "-chain", o.group)
		pm := paidLine.FindStringSubmatch(r.stdout)
		if pm == nil {
			t.Fatalf("history in a row:\n%s\n%s", r.stdout, r.stderr)
		}
		paid = append(paid, pm[1])
	}
	for i := 1; i < len(paid); i++ {
		if tx := h.chain.Tx(paid[i]); tx == nil || tx.Inputs[0].SourceTXID.String() != paid[i-1] {
			t.Fatalf("payment %d does not spend the change of payment %d", i, i-1)
		}
	}
	h.chain.SetHold(false)
	h.chain.Mine()
	r = h.must("payee", "payee", "settle", filepath.Join(a.state, "payments.jsonl"))
	if !strings.Contains(r.stdout, "3 payment(s) settled, 15 sat; 1 settled before") {
		t.Fatalf("payee settle of payments in a row:\n%s\n%s", r.stdout, r.stderr)
	}
	step("one coin pays three priced questions in one block, each payment spending the last one's unmined change; the payee settles all three")

	// Restart and restore: host a from its storage and rows.
	a.stop()
	a.start()
	if !strings.Contains(a.out.String(), "restored") {
		t.Fatalf("no restore line:\n%s", a.out)
	}
	r = h.must("alice", "open", o.group, "-v")
	if strings.Contains(r.stderr, "DISAGREES") {
		t.Fatalf("after the restart:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("bob", "", "open", o.group), exitRefused, "removed at seq 1")
	h.want(h.run("carol", "", "member", o.carol), exitRefused, "revoked")
	step("host a restarted: its index restored; the hosts still agree; bob still cannot open, carol stays revoked")

	// Unicast: every object to each host.
	h.env["BORG_MODE"] = "unicast"
	delete(h.env, "BORG_FACADE")
	dave := h.identity("dave", false)
	r = h.must("org", "org", "add", dave, "-group", o.group)
	if !strings.Contains(r.stdout, "removed "+o.carol+": rotation debt") || !strings.Contains(r.stdout, "epoch 3 (a new epoch") {
		t.Fatalf("unicast add:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, l := range hostsLine.FindAllString(r.stdout, -1) {
		if !strings.HasSuffix(l, "missed 0") {
			t.Fatalf("unicast tallies:\n%s", r.stdout)
		}
	}
	r = h.must("dave", "open", o.group, "-write")
	if !strings.Contains(r.stdout, "epoch   3 OPENED") || !strings.Contains(r.stdout, "write   under epoch 3") {
		t.Fatalf("dave:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("carol", "", "open", o.group), exitRefused, "removed at seq 3")
	step("unicast: org add dave (grant, admit, add): carol's debt removed first, epoch 3; dave opens it and has writer evidence; carol cannot")

	b.stop()
	h.want(h.run("org", "", "epoch", "rotate", o.group), exitUsage, "1 of 2 host(s) took it and the quorum is 2")
	b.start()
	r = h.must("org", "doctor")
	if !strings.Contains(r.stdout, "outbox      group-token") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	r = h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stderr, "publishing what a previous run persisted") || !strings.Contains(r.stdout, "epoch 5 (a new epoch") {
		t.Fatalf("resume:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, m := range []string{"alice", "dave"} {
		r := h.must(m, "open", o.group)
		if !strings.Contains(r.stdout, "epoch   5 OPENED") || strings.Contains(r.stderr, "DISAGREES") {
			t.Fatalf("%s:\n%s\n%s", m, r.stdout, r.stderr)
		}
	}
	step("b down: a token needs every host, so it stays persisted (exit 2); b back: the next command publishes it first; both agree")

	// A second home of the organisation, made around its identity key on
	// another machine, rebuilds from the hosts and goes on.
	r = h.must("org", "org", "signer")
	if !strings.Contains(r.stdout, "signer  ") || !strings.Contains(r.stdout, "epoch 5\n") {
		t.Fatalf("org signer:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.secondHome("org2")
	if r.code != 0 || !strings.Contains(r.stdout, "(1 new group(s))") || !strings.Contains(r.stdout, " 1 epoch key(s)") ||
		!strings.Contains(r.stdout, "epoch 4: the signer key was not a member of this epoch") {
		t.Fatalf("org sync on a second home:\n%s\n%s", r.stdout, r.stderr)
	}
	if got, want := sorted(h.must("org2", "org", "show").stdout), sorted(h.must("org", "org", "show").stdout); got != want {
		t.Fatalf("the second home differs:\n%s\n---\n%s", got, want)
	}
	r = h.must("org2", "epoch", "rotate", o.group)
	if !strings.Contains(r.stdout, "epoch 6 (a new epoch") {
		t.Fatalf("rotate from the second home:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("alice", "open", o.group)
	if !strings.Contains(r.stdout, "epoch   6 OPENED") || strings.Contains(r.stderr, "DISAGREES") {
		t.Fatalf("alice after the second home's rotation:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("org", "org", "sync")
	if !strings.Contains(r.stdout, "1 group token(s)") || !strings.Contains(r.stdout, " 1 epoch key(s)") {
		t.Fatalf("the first home catches up:\n%s\n%s", r.stdout, r.stderr)
	}
	step("org signer, then a second home made around the identity key: org sync rebuilds chains, roll and the epoch key from the real hosts; it rotates, and the first home catches up")
	t.Logf("e2e client: PASS (%d steps)", n)
}
