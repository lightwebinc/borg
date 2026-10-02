// Package borgrec is the byte-level contract of borg: the organisation and
// group records that ride mined chain tokens, the membership grant and the
// epoch-key wrap that ride unmined carriers, the hall and scope grammars, the
// BRC-369 epoch key, and the lookup question classes. docs/spec.md is the
// normative text; this package generates the golden vectors under
// testdata/vectors.
//
// Every record is canonical CBOR through bcommon's cbor package, so one value
// has one encoding a reader accepts. Every decoder bounds its input before the
// CBOR decoder runs, and the CBOR decoder bounds every declared length against
// the bytes present before it allocates.
package borgrec

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/keyed"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/record"
)

// Registry values (docs/frozen.md); frozen once the first record is
// published to a public host.
const (
	// ProtocolName is the BRC-43 protocol name, security level 1. BRC-43 key
	// derivation refuses a name under five characters, so the application
	// name alone ("borg") cannot be the protocol name.
	ProtocolName = "borg organisation"
	// KeyOrg locks an organisation chain token and signs its fields.
	KeyOrg = "org"
	// KeyGroup locks a group chain token and signs its fields.
	KeyGroup = "group"
	// KeyRecord locks a wrap carrier's record output, signs its field, and
	// locks the outputs of the trees that fund wraps.
	KeyRecord = "record"
	// KeyGrant does the same for grant carriers and the trees that fund
	// them, so that a host can tell a grant's funding from any other.
	KeyGrant = "grant"
	// KeyChain locks the outputs of the trees that pay for a chain's first
	// token; no carrier is ever funded under it.
	KeyChain = "chain"
	// KeyFund is the embedded wallet's funding key.
	KeyFund = "fund"

	// TopicPrefix and LookupService are the overlay names (BRC-87).
	TopicPrefix   = "tm_borg_"
	LookupService = "ls_borg"

	// GrantTypeString is hashed into the BRC-52 certificate type of a
	// membership grant by the rule of BRC-169 section 4.5.
	GrantTypeString = "borg membership grant v1"

	// ReleaseProtocolName is BRC-369 section 5.2's protocol for an epoch-key
	// wrap, at security level 2. It is BRC-369's, adopted, not borg's.
	ReleaseProtocolName = "keyed content release"

	// DomainSymmetric and DomainCommitment are BRC-369 section 2's domain
	// strings, adopted verbatim.
	DomainSymmetric  = keyed.DomainSymmetric
	DomainCommitment = keyed.DomainCommitment
)

// Tags and magics. The tag prefix "bo" belongs to borg.
var (
	TagPrefix = []byte("bo")
	// TagOrg is the first field of an organisation chain token.
	TagOrg = []byte{'b', 'o', 0x01}
	// TagFunding is the one field of a funding-tree output.
	TagFunding = []byte{'b', 'o', 0x02}
	// TagGroup is the first field of a group chain token.
	TagGroup = []byte{'b', 'o', 0x03}
	// MagicOrg is key 0 of an organisation record: "bo", 'o', version 1.
	MagicOrg = []byte{'b', 'o', 'o', 0x01}
	// MagicGroup is key 0 of a group record: "bo", 'g', version 1.
	MagicGroup = []byte{'b', 'o', 'g', 0x01}
	// MagicGrant is key 0 of a membership grant record: "bo", 'm', version 1.
	MagicGrant = []byte{'b', 'o', 'm', 0x01}
	// MagicWrap is key 0 of an epoch-key wrap record: "bo", 'w', version 1.
	MagicWrap = []byte{'b', 'o', 'w', 0x01}
)

// Derivations under [1, "borg organisation"], each with counterparty anyone,
// so a reader derives the key from the identity key alone.
var (
	Protocol         = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: ProtocolName}
	OrgDerivation    = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyOrg}
	GroupDerivation  = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyGroup}
	RecordDerivation = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyRecord}
	GrantDerivation  = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyGrant}
	ChainDerivation  = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyChain}
	FundDerivation   = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyFund}

	// ReleaseProtocol is BRC-369 section 5.2's derivation protocol.
	ReleaseProtocol = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: ReleaseProtocolName}
)

// GrantType is the BRC-52 certificate type of a membership grant:
// SHA-256 of GrantTypeString (BRC-169 section 4.5's derivation rule).
func GrantType() [32]byte { return sha256.Sum256([]byte(GrantTypeString)) }

// GrantTypeBase64 is GrantType as BRC-52 writes a type: standard base64.
func GrantTypeBase64() string {
	t := GrantType()
	return base64.StdEncoding.EncodeToString(t[:])
}

// Bounds. A reader accepts every record within them; a later version may
// raise a bound but never lower it (docs/frozen.md).
const (
	// MaxSafe is 2^53 - 1.
	MaxSafe = 1<<53 - 1
	// MaxTime is 9999-12-31T23:59:59Z, the last second RFC 3339 can write.
	MaxTime = 253402300799
	// MaxEpoch is the largest epoch: BRC-369 section 5.2 writes the epoch as
	// four bytes.
	MaxEpoch = 1<<32 - 1
	// SuffixLen is the length of a hall's random suffix.
	SuffixLen = 10
	// MaxHall is the longest hall identifier: BRC-87 caps a name at 50
	// characters and the topic prefix takes 8.
	MaxHall = 50 - len(TopicPrefix)
	// MaxHallName is the longest readable part of a hall identifier.
	MaxHallName = MaxHall - 1 - SuffixLen
	// MinHall is the shortest hall identifier.
	MinHall = 1 + 1 + SuffixLen
	// GroupIDLen is the length of a group identifier.
	GroupIDLen = 16
	// MaxName bounds an organisation's or a group's display name, in bytes.
	MaxName = 64
	// MaxDelta bounds each change list of one transition: grants admitted
	// or revoked, members added or removed.
	MaxDelta = 64
	// MaxRoll bounds the grants an organisation's roll holds.
	MaxRoll = 65536
	// MaxMembers bounds the members of one group (BRC-369 section 8.3: the
	// linear rekey serves groups of a few hundred).
	MaxMembers = 1024
	// MaxEntries bounds the wraps one wrap record carries.
	MaxEntries = 16
	// MaxOrgRecord and MaxGroupRecord bound an encoded chain-token record.
	MaxOrgRecord   = 8192
	MaxGroupRecord = 8192
	// MaxGrantRecord and MaxWrapRecord bound an encoded carrier record.
	MaxGrantRecord = 4096
	MaxWrapRecord  = 4096
	// MaxKeys bounds the entries of a record map, unknown keys included.
	MaxKeys = 64
	// MaxRole and MaxScope bound a grant's role and scope, in bytes.
	MaxRole  = 64
	MaxScope = 512
	// WrapLen is the exact length of one wrap: the BRC-78 serialization of a
	// 32-byte scalar (4 + 33 + 33 + 32 + 32 + 32 + 16).
	WrapLen = 182
	// KeyringEntryLen is the exact length of one master-keyring entry: a
	// 32-byte field revelation key under BRC-2 (32 + 32 + 16).
	KeyringEntryLen = 80
	// RevealLen is the length of one revealed field revelation key.
	RevealLen = 32
)

// GrantFieldNames are the BRC-52 field names of a membership grant, in the
// order BRC-52 serializes them (sorted).
var GrantFieldNames = []string{"expiry", "role", "scope"}

// The refusals. A host counts them by Reason.
var (
	ErrTooLarge   = record.ErrTooLarge
	ErrCBOR       = record.ErrCBOR
	ErrKeyType    = record.ErrKeyType
	ErrMagic      = record.ErrMagic
	ErrMissing    = record.ErrMissing
	ErrType       = record.ErrType
	ErrRange      = record.ErrRange
	ErrIdentity   = errors.New("borgrec: identity key is not a canonical compressed key")
	ErrHall       = errors.New("borgrec: hall identifier breaks the grammar")
	ErrName       = errors.New("borgrec: name is not 1 to 64 bytes of text")
	ErrList       = record.ErrList
	ErrChain      = errors.New("borgrec: seq, prev and epoch fields disagree")
	ErrMembers    = errors.New("borgrec: member list names the organisation or the anyone key")
	ErrFields     = errors.New("borgrec: grant fields, keyring or reveal are not exactly expiry, role and scope")
	ErrSignature  = chaintoken.ErrSignature
	ErrEntry      = errors.New("borgrec: wrap entry does not match the record")
	ErrRevocation = errors.New("borgrec: revocation outpoint is not the carrier's funding outpoint")
	ErrCert       = errors.New("borgrec: grant certificate signature does not verify")
	ErrReveal     = errors.New("borgrec: revealed field does not decrypt to a valid value")
	ErrTransition = errors.New("borgrec: transition breaks the chain rules")
	ErrRotation   = errors.New("borgrec: transition removes a member without a new epoch")
	ErrCount      = errors.New("borgrec: count disagrees with the predecessor and the change lists")
	ErrScope      = errors.New("borgrec: scope breaks the grammar")
	ErrExpiry     = errors.New("borgrec: expiry is not YYYY-MM-DDTHH:MM:SSZ")
	ErrRole       = errors.New("borgrec: role is not 1 to 64 bytes of text")
	ErrQuery      = errors.New("borgrec: question is not one of the defined classes")
)

// Reason is the fixed label a host counts a refusal by.
func Reason(err error) string {
	for _, e := range []struct {
		err error
		s   string
	}{
		{ErrTooLarge, "too-large"}, {ErrCBOR, "cbor"}, {ErrKeyType, "key-type"},
		{ErrMagic, "magic"}, {ErrMissing, "missing"}, {ErrType, "type"},
		{ErrRange, "range"}, {ErrIdentity, "identity"}, {ErrHall, "hall"},
		{ErrName, "name"}, {ErrList, "list"}, {ErrChain, "chain"},
		{ErrMembers, "members"}, {ErrFields, "fields"}, {ErrSignature, "signature-form"},
		{ErrEntry, "entry"}, {ErrRevocation, "grant-revocation"}, {ErrCert, "grant-signature"},
		{ErrReveal, "grant-reveal"}, {ErrTransition, "transition"}, {ErrRotation, "rotation"},
		{ErrCount, "count"}, {ErrScope, "scope"}, {ErrExpiry, "expiry"}, {ErrRole, "role"},
		{ErrQuery, "query"}, {ErrToken, "token"}, {ErrUnwrap, "undecryptable"},
		{ErrScalar, "undecryptable"}, {ErrCommitment, "commitment"},
		{ErrShape, "shape"}, {ErrBEEF, "beef"}, {ErrMineable, "mineable"}, {ErrUnlock, "unlock"},
		{ErrLock, "lock"}, {ErrFieldSig, "signature"}, {ErrFunding, "funding"}, {ErrUnmined, "unmined"},
		{ErrTokenInput, "token-input"}, {ErrNotBorg, "not-borg"},
	} {
		if errors.Is(err, e.err) {
			return e.s
		}
	}
	return "other"
}
