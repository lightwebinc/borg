package limits

import (
	"fmt"
	"time"

	"github.com/lightwebinc/borg/borgrec"
)

// The policy limits of docs/limits.md, as the borg command applies them.
// None is a wire format or a signed bound (docs/frozen.md): the record
// bounds every reader accepts are borgrec's, and these are what a client
// chooses to send and to ask, so a later version may change any of them
// without a new record version. A cap refuses the setting before anything
// is sent, with a message naming the setting and the limit; a warning is
// printed and the command goes on.

// The plane's packets: an object larger than one is carried as fragments
// of FragmentPayload bytes (the IPv6 minimum MTU of 1280 less the IPv6, UDP
// and object fragment headers).
const (
	FragmentPayload = 1128
	// OpenObjectBound is BRC-149's bound on an object submitted on the open
	// path; MaxObjectBound the authenticated path's. No plane admits more.
	OpenObjectBound = 1 << 20
	MaxObjectBound  = 8 << 20
	// DefaultObjectBound is the object bound assumed when none is set.
	DefaultObjectBound = OpenObjectBound
)

// Funding trees.
const (
	DefaultTreeCount   = 32
	MaxTreeCount       = 1000
	PlaneTreeCountWarn = 100
	// DefaultChainTreeCount and MaxChainTreeCount size a chain tree: a
	// chain's first token carries its chain tree whole.
	DefaultChainTreeCount = 8
	MaxChainTreeCount     = 32
	// DefaultTreeSats is each grant or record funding output's value. A
	// carrier carries its funding output's whole value and is never mined,
	// so one satoshi is enough.
	DefaultTreeSats = 1
	// ChainTreeSats is each chain-tree output's value: a chain's first
	// token spends one, and its value goes to the token's change.
	ChainTreeSats = 1
	// DefaultAhead is how few outputs a tree has left when the next is
	// minted ahead.
	DefaultAhead = 4
)

// Wraps and transitions.
const (
	// PlaneEntries and UnicastEntries are the default entries per wrap
	// record: an 8-entry carrier is 4 packets on the plane, and unicast has
	// no fragments.
	PlaneEntries   = 8
	UnicastEntries = borgrec.MaxEntries
	// MembersWarn is the group size above which a removal is warned about:
	// it costs at least 32 wrap carriers.
	MembersWarn = 256
	// PlaneChangesWarn is the most changes one token makes on the plane
	// before a warning: a token and its parent then pass 4 packets.
	PlaneChangesWarn = 16
	// DefaultExpiry is how long a grant is current unless -expiry says.
	DefaultExpiry = 365 * 24 * time.Hour
	// SignerExpiry is how long the grant of the organisation's own signer
	// key is current: it is renewed by the tooling, not by a person.
	SignerExpiry = 10 * 365 * 24 * time.Hour
	// ExpirySkew is the reader's allowance for clock skew (spec section
	// 5.2; BRC-169 section 9.6).
	ExpirySkew = 300 * time.Second
	// MaxGrantsPerSubject is the most grants one subject holds in a roll.
	MaxGrantsPerSubject = 8
)

// Publication and freshness.
const (
	// DefaultDelay is the propagation delay after a mined object's block
	// is in the publisher's headers, before it is published; MaxDelay its
	// cap.
	DefaultDelay = 60 * time.Second
	MaxDelay     = 10 * time.Minute
	// DefaultFreshness and MaxFreshness bound how old a writer's evidence
	// that a group's token is the latest may be (spec section 6.5).
	DefaultFreshness = 60 * time.Second
	MaxFreshness     = 600 * time.Second
)

// Hosts, quorum and requests.
const (
	MaxHosts         = 16
	UnicastHostsWarn = 5
	MinTimeout       = time.Second
	MaxTimeout       = 5 * time.Minute
	DefaultTimeout   = 15 * time.Second
)

// Retries are the waits between the tries of a submission: four tries,
// then the host is reported missed and the object stays persisted.
var Retries = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// PlaneWait are the waits between a publisher's lookups for a wrap it
// submitted to the plane, at each host it names, before it offers the wrap
// to that host directly: the plane delivers in well under a second.
var PlaneWait = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// Reading.
const (
	// PageSize is the most outputs one answer page holds (the spec's).
	PageSize = 64
	// MaxPages is the most pages one question reads per host: 4096 outputs.
	MaxPages = 64
	// DefaultMaxPrice is the most a priced question is paid without
	// -max-sats.
	DefaultMaxPrice = 1000
	// MaxAnswer is the most bytes a reader takes for one lookup answer: a
	// full page of the largest objects the plane carries fits several
	// times over.
	MaxAnswer = 16 << 20
	// SeqAnswers is the most outputs a reader takes from one answer to a
	// seq: a host keeps at most 8 tokens a seq, and what a hostile one
	// adds is not read.
	SeqAnswers = 16
	// DefaultBudget is the most a command that asks several priced
	// questions pays in all without -budget.
	DefaultBudget = 16 * DefaultMaxPrice
	// MaxBEEF is the most a reader takes for one answered object.
	MaxBEEF = 1 << 20
	// MaxUnminedPayments is how many of a payer's own payments may be
	// unmined at once with the next spending the change of the last: each
	// adds its transaction to the next payment's BEEF.
	MaxUnminedPayments = 16
)

// Publisher is a publisher's settings, as the checks see them.
type Publisher struct {
	Plane          bool
	Hosts          int
	Need           int
	TreeCount      int
	ChainTreeCount int
	Entries        int
	ObjectBound    int
}

func over(what string, v, limit any, why string) error {
	return fmt.Errorf("%s %v is over the limit of %v: %s (docs/limits.md)", what, v, limit, why)
}

// Check applies the caps, returning the first broken as an error, and the
// recommendations, returning each passed as a warning.
func (p Publisher) Check() (warnings []string, err error) {
	switch {
	case p.TreeCount < 1:
		return nil, fmt.Errorf("tree_count %d is under the limit of 1: a funding tree has at least one output (docs/limits.md)", p.TreeCount)
	case p.TreeCount > MaxTreeCount:
		return nil, over("tree_count", p.TreeCount, MaxTreeCount, "every carrier's BEEF carries its whole funding tree")
	case p.ChainTreeCount < 1:
		return nil, fmt.Errorf("chain_tree_count %d is under the limit of 1 (docs/limits.md)", p.ChainTreeCount)
	case p.ChainTreeCount > MaxChainTreeCount:
		return nil, over("chain_tree_count", p.ChainTreeCount, MaxChainTreeCount, "a chain's first token carries its chain tree whole")
	case p.Entries < 1:
		return nil, fmt.Errorf("entries %d is under the limit of 1 (docs/limits.md)", p.Entries)
	case p.Entries > borgrec.MaxEntries:
		return nil, over("entries", p.Entries, borgrec.MaxEntries, "the record bound every reader accepts")
	case p.ObjectBound > MaxObjectBound:
		return nil, over("object_bound", p.ObjectBound, MaxObjectBound, "no plane admits a larger object (BRC-149)")
	case p.Hosts > MaxHosts:
		return nil, over("hosts", p.Hosts, MaxHosts, "a unicast publisher uploads every object to each host, and a reader asks each")
	}
	if p.Plane {
		if p.TreeCount > PlaneTreeCountWarn {
			warnings = append(warnings, fmt.Sprintf("tree_count %d on the plane adds about %d bytes of funding tree to every carrier; %d is the default (docs/limits.md)", p.TreeCount, p.TreeCount*49, DefaultTreeCount))
		}
		if p.Entries > PlaneEntries {
			warnings = append(warnings, fmt.Sprintf("entries %d on the plane makes a wrap carrier of up to %d packets; %d is the default (docs/limits.md)", p.Entries, (p.Entries*184+2600)/FragmentPayload+1, PlaneEntries))
		}
	} else {
		if p.Hosts > UnicastHostsWarn {
			warnings = append(warnings, fmt.Sprintf("%d hosts in mode unicast: every object is uploaded %d times; beyond %d hosts the plane is what removes that cost (docs/limits.md)", p.Hosts, p.Hosts, UnicastHostsWarn))
		}
		if p.Need > 0 && p.Need < p.Hosts/2+1 {
			warnings = append(warnings, fmt.Sprintf("a quorum of %d of %d hosts is below a majority: a carrier counts as published while most hosts lack it; tokens and sweeps always need every host (docs/limits.md)", p.Need, p.Hosts))
		}
	}
	return warnings, nil
}
