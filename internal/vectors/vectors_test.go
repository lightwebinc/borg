package vectors

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/borg/borgrec"
)

const dir = "../../testdata/vectors"

// TestVectorsMatch regenerates every file and compares it byte for byte with
// the one checked in, and refuses a checked-in file nothing generates.
func TestVectorsMatch(t *testing.T) {
	files, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names(files) {
		have, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v (run: GOWORK=off go run ./cmd/vectors)", name, err)
			continue
		}
		if !bytes.Equal(have, files[name]) {
			t.Errorf("%s differs from what the codec generates", name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok {
			t.Errorf("%s is checked in but nothing generates it", e.Name())
		}
	}
}

func readHex(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	out, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHexFilesDecode reads the checked-in hex files as a second implementation
// would, without the generator: each decodes and re-encodes to itself.
func TestHexFilesDecode(t *testing.T) {
	for name, dec := range map[string]func([]byte) ([]byte, error){
		"org-genesis.hex":                 func(b []byte) ([]byte, error) { r, err := borgrec.DecodeOrg(b); return enc(r, err) },
		"org-offboard.hex":                func(b []byte) ([]byte, error) { r, err := borgrec.DecodeOrg(b); return enc(r, err) },
		"org-opaque.hex":                  func(b []byte) ([]byte, error) { r, err := borgrec.DecodeOrg(b); return enc(r, err) },
		"group-create.hex":                func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGroup(b); return enc(r, err) },
		"group-add.hex":                   func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGroup(b); return enc(r, err) },
		"group-remove.hex":                func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGroup(b); return enc(r, err) },
		"group-rotate.hex":                func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGroup(b); return enc(r, err) },
		"grant-alice.hex":                 func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGrant(b); return enc(r, err) },
		"grant-bob.hex":                   func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGrant(b); return enc(r, err) },
		"grant-carol.hex":                 func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGrant(b); return enc(r, err) },
		"grant-opaque.hex":                func(b []byte) ([]byte, error) { r, err := borgrec.DecodeGrant(b); return enc(r, err) },
		"wrap-create.hex":                 func(b []byte) ([]byte, error) { r, err := borgrec.DecodeWrap(b); return enc(r, err) },
		"wrap-add.hex":                    func(b []byte) ([]byte, error) { r, err := borgrec.DecodeWrap(b); return enc(r, err) },
		"wrap-remove.hex":                 func(b []byte) ([]byte, error) { r, err := borgrec.DecodeWrap(b); return enc(r, err) },
		"wrap-rotate.hex":                 func(b []byte) ([]byte, error) { r, err := borgrec.DecodeWrap(b); return enc(r, err) },
		"policy-def-releaser.hex":         func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-def-releaser-again.hex":   func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-def-moderator.hex":        func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-bind-releaser-group.hex":  func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-bind-releaser-bob.hex":    func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-bind-moderator-alice.hex": func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-bind-moderator-carol.hex": func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
		"policy-bind-archivist-carol.hex": func(b []byte) ([]byte, error) { r, err := borgrec.DecodePolicy(b); return enc(r, err) },
	} {
		b := readHex(t, name)
		re, err := dec(b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(re, b) {
			t.Errorf("%s does not re-encode to itself", name)
		}
	}
}

type encoder interface{ Encode() ([]byte, error) }

func enc[T encoder](r T, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return r.Encode()
}

// TestRefusalFile replays refusal-v1.json from the file alone.
func TestRefusalFile(t *testing.T) {
	var f struct {
		Refusals []refusalVec `json:"refusals"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "refusal-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Refusals {
		if r.Stage != "record" {
			continue
		}
		raw, _ := hex.DecodeString(r.Hex)
		var err error
		switch r.Record {
		case "org":
			_, err = borgrec.DecodeOrg(raw)
		case "group":
			_, err = borgrec.DecodeGroup(raw)
		case "grant":
			_, err = borgrec.DecodeGrant(raw)
		case "wrap":
			_, err = borgrec.DecodeWrap(raw)
		case "policy":
			_, err = borgrec.DecodePolicy(raw)
		}
		if got := borgrec.Reason(err); got != r.Reason {
			t.Errorf("%s %q: %q, want %q", r.Record, r.Label, got, r.Reason)
		}
	}
}

// TestQueryFile replays query-v1.json from the file alone.
func TestQueryFile(t *testing.T) {
	var f struct {
		Questions []struct {
			Question string `json:"question"`
			Class    string `json:"class"`
			Refused  bool   `json:"refused"`
		} `json:"questions"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "query-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.Questions {
		got, err := borgrec.ParseQuery([]byte(q.Question))
		if q.Refused != (err != nil) || (err == nil && got.Class.Name != q.Class) {
			t.Errorf("%s: %v", q.Question, err)
		}
	}
}

// TestAuthzFile replays authz-v1.json from the files alone: the policy set
// is read back from policy-v1.json's hex, and each subject's effective
// permissions and verdicts are recomputed.
func TestAuthzFile(t *testing.T) {
	var pf struct {
		Records []struct {
			Name string `json:"name"`
			Hex  string `json:"hex"`
		} `json:"records"`
	}
	readJSONFile(t, "policy-v1.json", &pf)
	byName := map[string]*borgrec.Policy{}
	for _, r := range pf.Records {
		b, err := hex.DecodeString(r.Hex)
		if err != nil {
			t.Fatal(err)
		}
		p, err := borgrec.DecodePolicy(b)
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		byName[r.Name] = p
	}
	var f struct {
		Now      string   `json:"now"`
		Set      []string `json:"set"`
		Subjects []struct {
			Name      string   `json:"name"`
			Key       string   `json:"key"`
			Member    bool     `json:"member"`
			Roles     []string `json:"roles"`
			Groups    []string `json:"groups"`
			Effective []string `json:"effective"`
			Asked     []struct {
				Permission string `json:"permission"`
				Allowed    bool   `json:"allowed"`
			} `json:"asked"`
		} `json:"subjects"`
	}
	readJSONFile(t, "authz-v1.json", &f)
	set := make([]*borgrec.Policy, 0, len(f.Set))
	for _, n := range f.Set {
		p, ok := byName[n]
		if !ok {
			t.Fatalf("the set names %q, which policy-v1.json does not hold", n)
		}
		set = append(set, p)
	}
	now, err := borgrec.ParseExpiry(f.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Subjects {
		key, err := hex.DecodeString(s.Key)
		if err != nil {
			t.Fatal(err)
		}
		sub := borgrec.Subject{Key: key, Member: s.Member, Roles: s.Roles}
		for _, g := range s.Groups {
			b, err := hex.DecodeString(g)
			if err != nil || len(b) != borgrec.GroupIDLen {
				t.Fatalf("%s: group %q", s.Name, g)
			}
			sub.Groups = append(sub.Groups, [borgrec.GroupIDLen]byte(b))
		}
		got := borgrec.Effective(sub, set, now)
		if strings.Join(got, ",") != strings.Join(s.Effective, ",") {
			t.Errorf("%s: effective %v, want %v", s.Name, got, s.Effective)
		}
		for _, a := range s.Asked {
			allowed, err := borgrec.Allows(got, a.Permission)
			if err != nil {
				t.Fatalf("%s asks %q: %v", s.Name, a.Permission, err)
			}
			if allowed != a.Allowed {
				t.Errorf("%s asks %q: %v, want %v", s.Name, a.Permission, allowed, a.Allowed)
			}
		}
	}
}

// readJSONFile reads a vector file as a second implementation would.
func readJSONFile(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}
