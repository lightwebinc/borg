package module

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/borg/borgrec"
)

// Reasons are every label a host counts a refused submission by: the
// record rules of spec section 3.1 (a token's record is read by its tag,
// so even magic is reachable), the grant's content rules, and the
// transaction rules of section 11.1. The member's labels (token,
// undecryptable, commitment), the grant's field-value labels (scope,
// expiry, role, which admission reports as grant-reveal) and query are
// never an admission verdict.
var Reasons = []string{
	"too-large", "cbor", "key-type", "magic", "missing", "type", "range", "identity", "hall", "name", "list", "chain", "members", "fields",
	"signature-form", "entry", "role", "permission", "binding", "count", "grant-revocation", "grant-signature", "grant-reveal", "transition",
	"rotation", "shape", "beef", "mineable", "unlock", "lock", "signature", "funding", "unmined", "token-input", "not-borg",
}

// AdmitKinds are what an admitted transaction was.
var AdmitKinds = []string{"grant", "policy", "wrap", "org-token", "group-token", "sweep"}

// RecentAdmissions is how many admitted txids a manager remembers to count
// each once.
const RecentAdmissions = 4096

// Refused is raised for every refusal. The engine counts it as a failed
// topic for this submission and stores nothing.
type Refused struct {
	Reason string
	Detail string
}

func (r *Refused) Error() string { return "borg: refused, " + r.Reason + ": " + r.Detail }

// ErrNoSubmission is a host that hands the manager a parsed BEEF without
// the submission's bytes (bcommon module.WithSubmission): the manager
// decides on the wire form (the counts a BEEF declares, which proofs it
// carries), which a parsed and merged BEEF no longer shows.
var ErrNoSubmission = errors.New("borg: the host did not hand the manager the submission's bytes (bcommon module.WithSubmission)")

// TopicManager is tm_borg_<name>_<suffix>: the topic manager of one hall
// (spec section 11.1), one for each configured hall. The rules are
// borgrec.Admit's; this is the engine seam: it runs them on the submission
// and turns the verdict into admittance instructions.
//
// The engine records every transaction a topic manager returns
// instructions for as applied, empty instructions included: a later
// submission of the same txid is a duplicate no topic manager is shown
// again. A refusal returned as empty instructions would therefore be
// permanent for that txid: anyone could suppress a genuine carrier by
// submitting it first in a different BEEF, and a token refused because the
// host's headers lagged would stay refused. So every refusal is raised,
// and the engine records nothing and marks nothing spent: a refused
// transaction offered again is decided again. No rule reads another topic
// or anything this topic holds: the previous coins decide only which spent
// outputs are retained.
type TopicManager struct {
	Topic string
	Hall  string
	host  bmodule.Host
	h     borgrec.Host

	mu     sync.Mutex
	recent map[string]bool
	order  []string
}

var _ bmodule.TopicManager = (*TopicManager)(nil)

// NewTopicManager makes the manager of one topic. The floors (262144
// bytes, 1024 inputs) are held where an operator sets the bounds
// (ParseConfig); a test may name the vectors' smaller ones.
func NewTopicManager(topic string, host bmodule.Host, headers chaintracker.ChainTracker, maxBEEF, maxSweepInputs int) (*TopicManager, error) {
	if !strings.HasPrefix(topic, borgrec.TopicPrefix) {
		return nil, fmt.Errorf("borg: %s is not a tm_borg_ topic", topic)
	}
	hall := topic[len(borgrec.TopicPrefix):]
	if err := borgrec.CheckHall(hall); err != nil {
		return nil, err
	}
	if maxBEEF < 1 || maxSweepInputs < 1 {
		return nil, fmt.Errorf("borg: a BEEF bound of %d and a sweep input bound of %d", maxBEEF, maxSweepInputs)
	}
	return &TopicManager{Topic: topic, Hall: hall, host: host, h: borgrec.Host{Hall: hall, Headers: headers, MaxBEEF: maxBEEF, MaxSweepInputs: maxSweepInputs},
		recent: map[string]bool{}}, nil
}

// Admit applies the rules to the submission's bytes with the held inputs,
// as the TypeScript manager's identifyAdmissibleOutputs does.
func (t *TopicManager) Admit(ctx context.Context, raw []byte, held []int) (overlay.AdmittanceInstructions, error) {
	a, err := borgrec.Admit(ctx, raw, held, t.h)
	if err != nil {
		reason := borgrec.Reason(err)
		if reason == "other" {
			return overlay.AdmittanceInstructions{}, err
		}
		t.host.Metrics().Inc("borg_refused_total", map[string]string{"reason": reason}, 1)
		detail := strings.TrimPrefix(err.Error(), "borgrec: ")
		t.host.Log("tm_borg refused", map[string]any{"topic": t.Topic, "reason": reason, "detail": err.Error()})
		return overlay.AdmittanceInstructions{}, &Refused{Reason: reason, Detail: detail}
	}
	txid := chainhash.Hash(a.Txid)
	t.count(a.Kind.String(), txid.String())
	ai := overlay.AdmittanceInstructions{OutputsToAdmit: make([]uint32, 0, len(a.Outputs)), CoinsToRetain: make([]uint32, 0, len(a.Retain))}
	for _, o := range a.Outputs {
		ai.OutputsToAdmit = append(ai.OutputsToAdmit, uint32(o)) //nolint:gosec // bounded by the transaction's outputs
	}
	for _, r := range a.Retain {
		ai.CoinsToRetain = append(ai.CoinsToRetain, uint32(r)) //nolint:gosec // bounded by the transaction's inputs
	}
	return ai, nil
}

// IdentifyAdmissibleOutputs implements bcommon's module.TopicManager over
// the submission's bytes in ctx.
func (t *TopicManager) IdentifyAdmissibleOutputs(ctx context.Context, _ *transaction.Beef, _ *chainhash.Hash, previousCoins []uint32) (overlay.AdmittanceInstructions, error) {
	raw, ok := bmodule.Submission(ctx)
	if !ok {
		return overlay.AdmittanceInstructions{}, ErrNoSubmission
	}
	held := make([]int, 0, len(previousCoins))
	for _, c := range previousCoins {
		held = append(held, int(c))
	}
	return t.Admit(ctx, raw, held)
}

// count counts an admission once per transaction. The engine shows a
// manager a txid it already applied only when two submissions of it race
// past its duplicate check, so borg_admitted_total counts distinct
// transactions and borg_admitted_repeats_total the rest. The memory is the
// last RecentAdmissions txids.
func (t *TopicManager) count(kind, txid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recent[txid] {
		t.host.Metrics().Inc("borg_admitted_repeats_total", map[string]string{"kind": kind}, 1)
		return
	}
	t.recent[txid] = true
	t.order = append(t.order, txid)
	if len(t.order) > RecentAdmissions {
		delete(t.recent, t.order[0])
		t.order = t.order[1:]
	}
	t.host.Metrics().Inc("borg_admitted_total", map[string]string{"kind": kind}, 1)
}

// GetDocumentation implements module.TopicManager.
func (t *TopicManager) GetDocumentation() string {
	return strings.Join([]string{
		"# " + t.Topic,
		"",
		"The borg hall " + t.Hall + ": grant and wrap carriers, each spending one output of",
		"its organization's mined funding tree; organization and group tokens, each mined",
		"and published with its parent; and the tombstones of mined sweeps (spec section",
		"11.1). Each is admitted on its own bytes and the host's headers; nothing the topic",
		"holds is read. Every refusal is raised, so nothing refused is recorded.",
	}, "\n")
}

// GetMetaData implements module.TopicManager.
func (t *TopicManager) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{Name: t.Topic, Description: "Admits the borg hall " + t.Hall + "."}
}
