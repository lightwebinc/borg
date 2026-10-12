# borg: the organization and group contract

borg gives an organization a membership roll and its groups a key that
changes whenever anyone leaves, on the BEEF object plane with overlay hosts
as replicas. An organization is an identity key. Its roll is a set of BRC-52
certificates, one per member, issued by the organization's key and committed
to by a chain of mined tokens. A group is a second chain of mined tokens:
every membership change is one spend of the chain, so changes are totally
ordered without a coordinator, and every host checks that a change which
removes someone begins a new **epoch**. Each epoch has its own key, sampled at
random and committed to before anyone holds it (BRC-369), and the
organization wraps it to each member in its own record on the plane. A
removed member receives no wrap of the next epoch's key and cannot derive it.
A host is an ordinary overlay host running the `tm_borg_<name>_<suffix>`
topic manager and the `ls_borg` lookup service.

This document is normative. MUST, MUST NOT, SHOULD and MAY are used as in
RFC 2119. Every byte-level and name-level choice that cannot change once a
record is on a public host is listed in [frozen.md](frozen.md). The golden
vectors in `testdata/vectors` are part of the contract (section 17).

## 1. Objects

| Object | On chain | On the plane | What it is |
| --- | --- | --- | --- |
| **Organization record** `O` | inside a mined token | the token's transaction | canonical CBOR: one transition of the roll (section 3) |
| **Group record** `G` | inside a mined token | the token's transaction | canonical CBOR: one transition of a group's membership; a token that begins an epoch is that epoch's descriptor (section 4) |
| **Grant record** `M` | never | inside a carrier | canonical CBOR: one membership grant, a BRC-52 certificate (section 5) |
| **Policy record** `P` | never | inside a carrier | canonical CBOR: one role definition or one binding of a role to a group or a key (section 5A) |
| **Wrap record** `W` | never | inside a carrier | canonical CBOR: one epoch's key wrapped to up to 16 members (section 6) |
| **Chain token** | mined | a BEEF object | a PushDrop of a tag and an `O` or a `G`; each spends the one before it (section 9.3) |
| **Carrier** `K` | never mined | a BEEF object | one unmined transaction whose one output holds an `M` or a `W` (section 9.2) |
| **Commitment** `C` | no | no | `C = txid(K)`, 32 bytes, hash byte order |
| **Funding tree** `F` | mined | inside each carrier's BEEF, and inside the BEEF of a chain's first token | one transaction of tagged outputs; each carrier spends one |
| **Sweep** | mined | a BEEF object | a mined spend of funding outputs; it retracts the carriers they funded and revokes the grants among them (section 9.5) |

State that needs an order is mined: the roll, the policy set and each group's
membership are chains of mined tokens, one transaction per transition.
Everything else rides an unmined carrier: grants, policy records and wraps
are facts about one transition, ordered by the token they belong to.

```text
 org key ── grant carriers M (one per member) ──▶ plane ──▶ hosts
    │       ── policy carriers P (roles, bindings) ──▶ plane ──▶ hosts
    │
    ├── org chain:   O_0 ──spend──▶ O_1 ──spend──▶ O_2      roll root over the grants' C,
    │                                                       policy root over the policy records' C
    │               (admit a,b,c)  (revoke b)
    │
    └── group chain: G_0 ──spend──▶ G_1 ──spend──▶ G_2 ──spend──▶ G_3
                    (add a,b;      (add c;        (remove b;     (rotate:
                     epoch 0, k0)   epoch 0)       epoch 1, k1)   epoch 2, k2)
                        │              │              │              │
                      W: k0→a,b      W: k0→c        W: k1→a,c      W: k2→a,c
```

A **hall** is the rendezvous: one topic, `tm_borg_<name>_<suffix>`, that a
set of hosts carries and any number of organizations share (section 10.1).
Every record names its hall, and an organization's chains and carriers all
live in one hall.

## 2. Conventions

- **CBOR.** Every record is one CBOR map in the core deterministic encoding
  of RFC 8949 section 4.2.1, written and read by bcommon's `cbor` package,
  which enforces it on encode and on decode. The subset is unsigned and
  negative integers, byte strings, UTF-8 text strings, arrays, maps, `false`,
  `true` and `null`; tags, floats, indefinite lengths and nesting deeper than
  16 are refused. One value has exactly one encoding a reader accepts.
- **Record keys.** A record's top-level keys are unsigned integers, at most 64
  of them. A key this version defines is checked as its table says. A key
  above the last defined one is unknown: a reader MUST preserve it verbatim
  when it re-encodes and MUST otherwise ignore it.
- **Hashes.** SHA-256 unless stated. A txid or commitment inside a record is
  in hash byte order (the order SHA-256d produces). A txid in JSON (a lookup
  question, a cursor, an outpoint) is in display order, the byte-reversed hex
  the engine and every explorer print. Both are lowercase hex.
- **Keys.** An identity key is a compressed secp256k1 point: 33 bytes, prefix
  `0x02` or `0x03`, an x-coordinate below the field prime, and a point on the
  curve (bcommon `guard.ParsePubKey`). A reader MUST refuse any other byte
  string, including an x-coordinate at or above the prime that some parsers
  reduce and accept, because that would give one identity two names. In JSON
  a key is its 66 lowercase hex digits. The **anyone key** is BRC-43's anyone
  counterparty, the generator point (private key 1); nothing is ever released
  to it.
- **Lists of keys and commitments** in a record are strictly ascending by
  their bytes, so one set has one encoding.
- **Signatures** a host checks are strict DER with a low S value: the bytes
  MUST equal the one canonical serialization of the signature's `(r, s)`.
- **Time.** Times are Unix seconds (UTC) on the writer's clock, from 1 to
  253402300799 (`9999-12-31T23:59:59Z`). They are claims. A host never admits
  or refuses by a time; only a reader's checks of a grant's expiry and of
  freshness use the clock (sections 5.2 and 13).

## 3. The organization record

Magic `"boo"` `0x01`. The second field of an organization token.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 6f 6f 01` (`"bo"`, `'o'` for organization, version 1). Always the first key |
| 1 | `hall` | text; the hall identifier grammar (section 10.1) | MUST equal the topic's hall |
| 2 | `org` | bytes; the key rule | the organization's identity key. The token's lock is derived from it (section 9.3), so it is bound, not claimed |
| 3 | `seq` | uint, 0 to 2^53 - 1 | 0 for the organization's first token, then one more per token |
| 4 | `prev` | bytes(32) | the txid, hash byte order, of the mined transaction holding the token at `seq - 1`; all zero exactly when `seq = 0` |
| 5 | `name` | text, 1 to 64 bytes | the organization's display name |
| 6 | `admit` | array of 0 to 64 bytes(32), ascending | grant commitments entering the roll |
| 7 | `revoke` | array of 0 to 64 bytes(32), ascending, disjoint from `admit` | grant commitments leaving the roll |
| 8 | `count` | uint, 0 to 65536 | the number of grants in the roll after this transition |
| 9 | `roll` | bytes(32) | the roll root after this transition (section 3.2) |
| 10 | `opaque` | bool | `true` when the organization's grants carry no public reveal (section 5.4) |
| 11 | `created` | uint, 1 to 253402300799 | a claim |

| 12 | `define` | array of 0 to 64 bytes(32), ascending | policy commitments entering the policy set |
| 13 | `retire` | array of 0 to 64 bytes(32), ascending, disjoint from `define` | policy commitments leaving it |
| 14 | `policyCount` | uint, 0 to 65536 | the number of policy records in the set after this transition |
| 15 | `policy` | bytes(32) | the policy root after this transition (section 3.3) |
| 16 | `successor` | bytes; the key rule, or `null` | the key that may carry the chain on from the next token (section 3.4). `null` when the organization announces no handover |

Keys 0 to 16 are always present; keys 17 and above are unknown to this
version. The encoded record is at most 8192 bytes.

### 3.1 The refusal order

A record that breaks several rules is refused for the first, so that every
implementation reports the same reason. For each of the five records:

1. `too-large`: the encoded length is over the record's bound;
2. `cbor`: the bytes are not exactly one canonical item of the subset, or the
   item is not a map;
3. `too-large`: the map has more than 64 entries;
4. `key-type`: a key is not an unsigned integer;
5. `magic`: key 0 is absent, is not a byte string, or is not the record's
   magic;
6. each defined key in ascending order: `missing` if it is absent, then
   `type` if it has the wrong CBOR type, then the key's own rule: `hall` for
   the hall, `identity` for a key, `name` for a display name, `list` for a
   change list that is too long, not ascending, holds a duplicate or an
   element of the wrong shape, `identity` for a key inside a key list,
   `fields` for the grant's field, keyring and reveal maps,
   `signature-form` for the grant's signature, `entry` for a wrap, `role` for
   a role name, `permission` for a permission string, and
   `range` for every other bound in the record's table;
7. across fields, in this order: `list` when a value is both added and
   removed (or admitted and revoked); `members` when a group adds the
   organization or the anyone key, or a grant's subject is either; `chain`
   when `prev` is zero at a `seq` other than 0 or not zero at `seq` 0, when
   a group's `epochSeq` is after its `seq`, or when a group's first token is
   not epoch 0 at `epochSeq` 0; `binding` when a policy record's `kind` does
   not match its `to`, `expiry` or `permissions` (section 5A.1); `count` when
   a first token's `count` is not the length of its `admit` or `add` list, or
   the first token removes anything, or a first token's `policyCount` is not
   the length of its `define` or it retires anything.

`refusal-v1.json` pins every step case by case.

### 3.2 The roll

The roll after a token is the roll before it (empty before `seq` 0), less
`revoke`, plus `admit`. Its root is bcommon `commit`'s RFC 6962 Merkle Tree
Hash over the grant commitments in ascending byte order: leaf datum `C`,
leaf hash `SHA-256(0x00 || C)`, node `SHA-256(0x01 || left || right)`, the
empty roll's root `SHA-256()` of nothing. The leaf datum is the commitment,
the grant carrier's txid, because that is what the plane, the engine's storage and the
lookup answer already key a grant by, and because it binds the whole carrier.

A host cannot check `roll` or `count` against the set while it admits the
token: it reads only the predecessor (section 11.1). The index folds the
change lists along the chain and reports a token whose fold does not
reproduce `roll`, or that revokes a commitment not in the roll or admits one
already in it, as `roll-mismatch` (section 11.2). A reader that fetches the
roll checks its root itself (section 13).

### 3.3 The policy set

An organization carries a second set beside its roll: its **policy set**, the
policy records (section 5A) it has published and not retired. The set after a
token is the set before it (empty before `seq` 0), less `retire`, plus
`define`. Its root is bcommon
`commit`'s RFC 6962 Merkle Tree Hash over the policy commitments in ascending
byte order, exactly as the roll's root is computed over grant commitments
(section 3.2), and the empty set's root is `SHA-256()` of nothing.

The roll and the policy set are two roots on one chain, so a change of
membership and a change of policy are ordered against each other by the
chain that carries both: there is no moment at which a host or a reader holds
a new role and an old roll, or the other way round. One transition may do
both.

As with the roll, a host cannot check `policy` or `policyCount` against the
set while it admits a token (section 11.1): it reads only the predecessor.
Admission checks the arithmetic of `policyCount`; the index folds the change
lists along the chain and reports a token whose fold does not reproduce
`policy`, or that retires a commitment the set does not hold or defines one
it already holds, as `policy-mismatch` (section 11.2). A reader that fetches
the set checks its root itself (section 13).

### 3.4 Succession

The `successor` key announces a handover of the chain. When a
token names a successor, the next token of the chain MAY carry that key as
its `org`; it is then locked to `derive(successor, "org")` and its field
signature verifies under that key, while its input spends the announcing
token's output, which only the predecessor's key can unlock. Both keys
therefore sign the handover: the predecessor's to spend, the successor's to
claim. A token whose `org` differs from its predecessor's is refused
(`transition`) unless the predecessor's `successor` equals it, so a host
checks a handover from the bytes of the token and its parent alone. A chain's
first token may name a successor like any other, and no token can hand over
to a key its predecessor did not name, so every handover is one transition
announced and one transition taken.

A chain's `hall` and its position in the hall do not change, so the
organization's history is one chain across its keys. The organization's **key
history** is the sequence of `org` values along its canonical chain; a reader
treats a grant, a policy record or a wrap whose `org` is any key of that
history as the organization's, and the organization's **current key** is its
latest token's `org`.

Three things do not follow the key, and the organization's tooling carries
them over after a handover:

- **Groups.** Each group chain hands over the same way, with the group
  record's own `successor` (section 4). Group identifiers,
  epochs and history survive a handover, so what other applications have
  bound to a group stays bound.
- **The signer key.** The signer key is derived from the organization's
  identity key (section 7.4), so a handover gives the tooling a new signer
  key. Every group whose members include the predecessor's signer key
  removes it and adds the successor's, which begins a new epoch in each
  (section 4.2). That is the intended effect: the predecessor's key loses
  what it could read.
- **Grants, policy records and their trees.** The carriers the predecessor
  issued stay owned by the predecessor's key, and so do the funding trees
  whose outputs revoke and retire them. Until the successor re-issues them
  on its own trees, a revocation or a retirement needs the predecessor's key
  (section 16.15).

Succession is for a handover both keys take part in: a new device, a new
custodian, a planned retirement of a key. A key lost or compromised before
it announces a successor cannot hand over, and that organization is still a
new organization (section 16.6).

## 4. The group record

Magic `"bog"` `0x01`. The second field of a group token. Succession
(section 3.4) applies to a group chain as it does to the organization's.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 6f 67 01` (`"bo"`, `'g'` for group, version 1) |
| 1 | `hall` | text; the hall grammar | MUST equal the topic's hall |
| 2 | `org` | bytes; the key rule | the organization's identity key; the token is locked to it |
| 3 | `group` | bytes(16) | the group identifier, drawn uniformly at random by the organization when it creates the group and fixed for the group's life |
| 4 | `seq` | uint, 0 to 2^53 - 1 | 0 for the group's first token |
| 5 | `prev` | bytes(32) | txid of the token at `seq - 1`, hash byte order; zero exactly at `seq` 0 |
| 6 | `name` | text, 1 to 64 bytes | the group's display name |
| 7 | `epoch` | uint, 0 to 2^32 - 1 | the epoch in force after this transition |
| 8 | `epochSeq` | uint, 0 to `seq` | the `seq` of the token that began this epoch |
| 9 | `commitment` | bytes(32) | the epoch key's commitment, BRC-369 section 2.2's digest form (section 6.1) |
| 10 | `add` | array of 0 to 64 keys, ascending | members added |
| 11 | `remove` | array of 0 to 64 keys, ascending, disjoint from `add` | members removed |
| 12 | `count` | uint, 0 to 1024 | the number of members after this transition |
| 13 | `members` | bytes(32) | the members root after this transition (section 4.3) |
| 14 | `rotateOnJoin` | bool | when `true`, the next transition that adds anyone begins an epoch |
| 15 | `rotateEvery` | uint, 0 to 2^32 - 1 | seconds between scheduled rotations the organization intends; 0 means none. Informative: no host enforces it |
| 16 | `created` | uint, 1 to 253402300799 | a claim |

| 17 | `successor` | bytes; the key rule, or `null` | the key that may carry this group's chain on from the next token (section 3.4). `null` when no handover is announced |

Keys 0 to 17 are always present; keys 18 and above are unknown to this
version. The encoded record is at most 8192 bytes. Neither the organization
nor the anyone key is ever a member.

### 4.1 Epochs

An epoch is a numbered generation of the group's key. Epoch 0 begins at the
group's first token. A token **begins an epoch** when its `epochSeq` equals
its `seq`; its record is then that epoch's descriptor in BRC-369's sense: it
carries the epoch number, the commitment to the epoch's key, and through
`prev` and `epochSeq` the link to the epoch before it (BRC-369 section 8.1
rule 5's `previous`). The **epoch id** is SHA-256 of that token's encoded
group record, the place BRC-369 section 4.2 rule 3's content id takes. Every
other token of the epoch repeats its `epoch`, `epochSeq` and `commitment`.

### 4.2 Transitions

A group token at `seq > 0` spends the group token at `seq - 1` (section 9.3),
and a host checks the new record against the one it spends (the
**predecessor**, read from the predecessor's transaction, which the token's
BEEF carries):

1. `transition`: `hall` and `group` equal the predecessor's, `seq` is
   the predecessor's plus 1, `prev` is the txid of the transaction the
   predecessor's output is in, and `org` equals the predecessor's `org` or,
   where the predecessor names a `successor`, that successor (section 3.4);
2. `rotation`: the transition **must rotate** when `remove` is not empty, or
   when the predecessor's `rotateOnJoin` is `true` and `add` is not empty. A
   transition that must rotate has `epoch` one more than the predecessor's;
   any other has the predecessor's `epoch` or one more;
3. `rotation`: when `epoch` is the predecessor's, `commitment` and `epochSeq`
   equal the predecessor's; when it is one more, `commitment` differs from
   the predecessor's and `epochSeq` equals `seq`. `transition` when `epoch`
   is anything else;
4. `count`: `count` equals the predecessor's `count` plus the length of `add`
   less the length of `remove`.

Rule 2 is the property the application exists for, checked by every replica:
**no removal is admitted without a new epoch, and no new epoch is admitted
with the old epoch's commitment.** A host cannot check that the new key is
fresh, only that its commitment differs from the one before; freshness is the
organization's obligation (section 6.1). The index reports a commitment that
repeats any earlier epoch's as `commitment-reuse`, and a reader that holds
earlier commitments checks that none repeats (section 13). `group-v1.json` pins every
rule with an admitted and a refused case.

A transition that adds a key which an earlier token of the same group
removed MUST begin a new epoch, whatever the predecessor's `rotateOnJoin`:
otherwise the returning key receives the current epoch's key, and with it
everything written while it was away. A host cannot check this while it
admits, because the predecessor's record names only the members root, not
the keys ever removed; the organization's tooling MUST rotate, and the index
reports a token that re-adds a removed key without beginning an epoch as
`rejoin-unrotated` (section 11.2).

### 4.3 Members

The members after a token are the members before it (none before `seq` 0),
less `remove`, plus `add`. The members root is bcommon `commit`'s RFC 6962
root over the leaf data `SHA-256(K)`, one per member key `K`, in ascending order
of the keys' bytes (leaf hashes `SHA-256(0x00 || SHA-256(K))`); the empty
set's root is `SHA-256()` of nothing. As with
the roll (section 3.2), a host cannot check the root while it admits; the
index folds and reports `members-mismatch` for a token whose fold removes a
non-member, adds a member, or does not reproduce `count` and `members`, and
`rejoin-unrotated` for a token that adds a key an earlier token removed
without beginning an epoch (section 4.2). A reader treats a group whose
canonical chain holds a mismatched token as unusable from that token on: it
fails closed. It shows a group whose chain holds a `rejoin-unrotated` token
as inconsistent, and treats nothing keyed under that token's epoch as hidden
from the returning key.

Membership within one epoch only grows, because every removal begins a new
epoch. So the members of a group at its latest token are exactly the
recipients of that epoch's wraps when the organization has published them
all (section 6.3), which is how a reader enumerates them (section 13).

## 5. The membership grant

Magic `"bom"` `0x01`. The payload of a grant carrier's one output.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 6f 6d 01` (`"bo"`, `'m'` for membership, version 1) |
| 1 | `hall` | text; the hall grammar | MUST equal the topic's hall |
| 2 | `org` | bytes; the key rule | the certificate's certifier, and the carrier's owner |
| 3 | `subject` | bytes; the key rule | the member's identity key; neither the organization nor the anyone key |
| 4 | `serial` | bytes(32) | the certificate's serial number |
| 5 | `revocation` | array `[bytes(32), uint 0 to 2^32 - 1]` | the certificate's revocation outpoint: txid in hash byte order and output index. It MUST be the outpoint the carrier's one input spends (section 9.2) |
| 6 | `fields` | map of text to bytes | exactly `expiry`, `role` and `scope`: each field's ciphertext (section 5.2) |
| 7 | `signature` | bytes, 8 to 72 | the certifier's signature, strict low-S DER |
| 8 | `keyring` | map of text to bytes(80) | exactly the three names: each field revelation key encrypted to the subject (section 5.5) |
| 9 | `reveal` | map of text to bytes(32), optional | when present, exactly the three names: each field revelation key in the clear (section 5.4) |

Keys 0 to 8 are always present, key 9 is optional, and keys 10 and above are
unknown to this version. The encoded record is at most 4096 bytes.

### 5.1 The certificate

A grant is a BRC-52 identity certificate laid out as CBOR. The certificate a
BRC-52 verifier sees is:

| BRC-52 element | Value |
| --- | --- |
| `type` | `base64(SHA-256("borg membership grant v1"))` = `poEEbYGwpJbTzKTeSRBJzV31s8PNknNycAbyjDV3ALc=`, derived by BRC-169 section 4.5's rule |
| `serialNumber` | base64 of `serial` |
| `subject` | `subject` |
| `certifier` | `org` |
| `revocationOutpoint` | `revocation` |
| `fields` | each name to the standard padded base64 of its ciphertext |
| `signature` | `signature` |

The signature is BRC-52's: the certifier's wallet signs the certificate's
binary form without the signature (go-sdk `Certificate.ToBinary(false)`,
ts-sdk `toBinary(false)`) under protocol `[2, "certificate signature"]`, key
id `"<type> <serialNumber>"` in base64, counterparty anyone; a verifier checks
it with counterparty the certifier. Because the fields enter the binary form
as base64 text, the record stores the ciphertext bytes and a verifier writes
them back in standard padded base64, the one spelling. `grant-v1.json` carries
each grant's binary form, so an implementation can compare its own bytes
before it checks a signature.

The type is borg's own rather than BRC-169's delegation certificate: a
membership names a role and a fail-closed scope for one organization's
members, and BRC-52 expects certificates of one type to "use the same field
names and meanings", so other meanings take another type. The field names it shares with the
delegation certificate, `scope` and `expiry`, mean what they mean there.

### 5.2 Fields

| Field | Value in the clear | Rule |
| --- | --- | --- |
| `expiry` | text | exactly `YYYY-MM-DDTHH:MM:SSZ`, a real UTC time from 1970 (BRC-169 section 9.1's `expiry`, its spelling pinned). A grant is not current at or after it, allowing a reader's clock skew of at most 300 seconds (BRC-169 section 9.6). An organization renews by issuing a new grant |
| `role` | text, 1 to 64 bytes of UTF-8 | the role the organization gives the subject: it names a role whose permissions the organization's policy set defines, and a service that can read the field reads them (sections 5A.1 and 5A.4). A value that is not a well-formed role name names no role, and so authorizes nothing |
| `scope` | text, 1 to 512 bytes | section 5.3 |

Each field's ciphertext is BRC-52's: AES-256-GCM under the field's revelation
key, a fresh random 32-byte key per field per grant, written as
`IV || ciphertext || tag` with a 32-byte IV and a 16-byte tag (BRC-52's field
encryption). The
ciphertext of `expiry` is therefore exactly 68 bytes, of `role` 49 to 112
and of `scope` 49 to 560, and a host refuses any other length (`fields`).

### 5.3 Scope

`scope` follows BRC-169 section 9.2's grammar:

```abnf
scope       = scope-token *( "," scope-token )
scope-token = verb [ ":" qualifier ]
verb        = 1*32( lower / DIGIT / "-" )
qualifier   = 1*64( lower / DIGIT / "-" / "_" / "." )
```

with one restriction: the whole-scope `*` is refused, because a membership
grant never confers unrestricted authority. This version defines two verbs,
neither with a qualifier:

| Token | Meaning |
| --- | --- |
| `member` | the subject is a member of the organization. A grant whose scope does not grant `member` is not a membership, and its subject belongs to no group |
| `admin` | the subject administers the organization's tooling. A client shows it; it authorizes nothing on chain in this version, where every chain transition is signed by the organization's key |

Unknown tokens fail closed (BRC-169 section 9.2 rule 4), read as follows: a
verb this version does not define authorizes nothing, and a defined verb that
carries any qualifier is **denied**, never read as the bare verb. So
`member:eng` grants no membership, and `member,member:eng` grants none
either. A host checks only the grammar (when the scope is revealed); the
meaning is a reader's, so that a grant naming a later version's token is
admitted by an earlier host and simply authorizes nothing there.
`scope-v1.json` pins the grammar and the meaning.

### 5.4 Reveal, and the opaque setting

BRC-52 encrypts every field, and the subject reveals fields to a named
verifier through a keyring. borg's roll is meant to be checkable by hosts and
by anyone who reads the hall, so by default a grant carries `reveal`: each
field revelation key in the clear. It does what a BRC-52 keyring does for one
verifier, for everyone; a BRC-52 keyring itself encrypts each key to a named
verifier, and a key revealed to everyone needs no encryption. A host that
finds `reveal` checks that every key opens its field and that every value
meets its rule (`grant-reveal`).

An organization whose `opaque` is `true` publishes grants without `reveal`.
`opaque` is fixed at the organization's first token (section 11.1).
Its hosts and readers then cannot see roles or scopes, only that a grant
exists for a subject: the opaque setting hides roles and scopes, not
membership; its members read their own fields through the keyring
(section 5.5) and reveal them to a verifier the ordinary BRC-52 way, over
BRC-103 and BRC-104. A reader of a non-opaque organization treats a grant
without `reveal` as not current: it cannot check the scope. Group
membership is not hidden by the opaque setting: a wrap names its recipient
(section 16).

### 5.5 Keyring and custody

`keyring` is BRC-52's master keyring for the subject: each field revelation
key encrypted by the certifier to the subject under protocol
`[2, "certificate field encryption"]`, key id the field name, BRC-2's
symmetric form (80 bytes for a 32-byte key). The subject decrypts it with
counterparty the organization and opens every field; go-sdk's
`certificates.DecryptFields` does exactly this, and the vectors check it.

The grant carries everything a BRC-100 wallet's `acquireCertificate` takes
under the direct protocol (type, certifier, serial number, revocation
outpoint, signature, fields, the keyring for the subject, and the
organization as keyring revealer). A member's client MUST NOT keep a derived
summary in place of its grant (BRC-169 section 4.6): a client on a BRC-100
wallet acquires it with `acquireCertificate`; a client that is its own home
(the borg command's embedded wallet) keeps the grant record whole, as
carried, and rebuilds the certificate from it.

### 5.6 Revocation

A grant is revoked two ways, and an organization does both:

- **by omission:** the next organization token revokes the grant's
  commitment, so the roll no longer holds it. Readers of borg's records see
  it at once, and one token revokes up to 64 grants;
- **by spend:** a mined sweep spends the grant's revocation outpoint, which
  is the funding outpoint its carrier spends (section 9.5). Every BRC-52
  verifier that checks the outpoint sees it spent ("If that UTXO has been
  spent, the certificate MUST be treated as revoked"), and every host
  retracts the carrier. One sweep spends many outpoints. Hosts see a spend
  only when it is a sweep (a mined transaction whose output 0 is a
  funding-shaped tombstone, section 11.1), so an organization MUST revoke by
  sweep; any other mined spend of the outpoint revokes the grant for BRC-52
  verifiers while hosts keep answering it. Only a mined spend revokes: an
  unmined transaction spending the outpoint revokes nothing.

A grant is **current** for a reader when its commitment is in the roll of the
organization's canonical latest token, its revocation outpoint is unspent,
its scope grants `member`, and its expiry has not passed. A reader allows
300 seconds of clock skew on expiry (a grant is current until 300 seconds
after it); the organization's own tooling allows none, so it stops wrapping
to a key and owes its removal from the instant the grant expires by its
clock. Whether an outpoint
is spent comes from an index, never from SPV alone, and inherits the index's
honesty and lag (BRC-169 section 4.2); for anything that moves value or
releases a key, "unspent" needs a positive answer (section 13), never the
absence of a spend. For the organization's own grants, its tooling's record
of the sweeps it made is that evidence: a grant outpoint the organization
never swept is unspent by its own record, because only its key can spend it
(the grant tree is locked to its derived key), and one it swept is spent
from the moment it signed the sweep.

An organization funds each grant from its own funding output and makes one
carrier per output: two carriers on one outpoint are equivocation, and a
host answers only one of them (section 11.2). It holds at most 8 grants for
one subject in its roll at a time.

Changing the role **named in a grant** is a revocation and a fresh grant
(BRC-169 section 9.8): one organization token admits the new commitment and
revokes the old one. The member's groups do not change, because a group names
identity keys, not grants. There is a second way, which touches no grant:
bind a role to the member's key or to one of its groups, and retire that
binding to take it away (section 5A). What a role may do
changes without either, because that is in the policy set and not in anyone's
certificate.

## 5A. The policy record

Magic `"bop"` `0x01`. The payload of a policy carrier's one output. A policy
record is one of two things: a **role definition**, which names a role and
the permissions it carries, or a **binding**, which gives a role to a group
or to one key. The organization's token commits to the set of them by the
policy root (section 3.3).

This section carries a letter rather than a number because the sections after
it are cited by other specifications and by documents that are signed; the
policy record was added after they were, and renumbering them would make
every one of those citations point somewhere else.

Policy says what a member may do; the roll and the group chains say who a
member is. Keeping both on one chain is what makes a decision answerable:
every permission a service grants is a permission some mined token committed
to, at a `seq` a reader can name, over a roll the same token committed to.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 6f 70 01` (`"bo"`, `'p'` for policy, version 1). Always the first key |
| 1 | `hall` | text; the hall identifier grammar (section 10.1) | MUST equal the topic's hall |
| 2 | `org` | bytes; the key rule | the organization's identity key, and the carrier's owner. The carrier's lock is derived from it (section 9.2), so it is bound, not claimed |
| 3 | `kind` | uint, 0 or 1 | `0` a role definition, `1` a binding |
| 4 | `role` | text; the role-name grammar (section 5A.1) | the role this record defines or binds |
| 5 | `permissions` | array of 0 to 24 text, ascending, unique, each a permission (section 5A.2) | a definition's permissions, or a binding's attenuation (section 5A.1) |
| 6 | `to` | bytes(16), bytes(33) or `null` | a binding's subject: a group identifier or an identity key. `null` in a definition |
| 7 | `expiry` | text, exactly `YYYY-MM-DDTHH:MM:SSZ`, or `null` | a binding's expiry, a claim read against the reader's clock as a grant's is (section 5.2). `null` in a definition, and in a binding that does not expire |
| 8 | `created` | uint, 1 to 253402300799 | a claim |

Keys 0 to 8 are always present; keys 9 and above are unknown to this version.
The encoded record is at most 8192 bytes: 24 permissions of the grammar's
full length take about 6.1 KB (section 14). A policy record is in the clear:
unlike a grant's fields, nothing in it is encrypted, so every host and every
follower of the hall reads an organization's roles and bindings (section
16.16).

### 5A.1 The two kinds

| | `kind` 0, a role definition | `kind` 1, a binding |
| --- | --- | --- |
| `role` | the role being defined | the role being given |
| `permissions` | the permissions the role carries. An empty list defines a role that authorizes nothing, which a client may still show | the **attenuation**: the permissions this binding keeps of the role's. An empty list keeps the whole role |
| `to` | `null` | the group identifier (16 bytes) or identity key (33 bytes) the role is given to |
| `expiry` | `null` | the time after which the binding authorizes nothing, or `null` |

A record whose `kind` and fields disagree is refused `binding` (section 3.1
step 7): `kind` 0 with a `to` or an `expiry` that is not `null`, or `kind` 1
with a `to` that is `null`. A binding's `to` of 33 bytes meets the key rule
and is neither the organization nor the anyone key (`identity`); a `to` of 16
bytes is read as a group identifier, and no rule here ties it to a group that
exists (section 5A.5).

More than one definition of one role may be in the set at once: the
organization has equivocated, and a reader takes the **intersection** of
their permissions, which is the only reading that cannot grant more than the
organization meant by either. The tooling never publishes a second definition
of a role it already defines; it retires the first in the same transition
that defines the second.

A **role name** is an identifier, not a label:

```abnf
role-name = 1*64( lower / DIGIT / "-" / "_" / "." )
lower     = %x61-7A
```

the same characters BRC-169 section 9.2 allows a scope qualifier. Names are
compared byte for byte. Lower case and ASCII mean that two roles cannot be
shown alike and read apart, which no normalization rule achieves by itself;
the organization's display names (`name`) carry its human wording. A grant's
`role` field that is not a well-formed role name names no role, so a role
written as a display label carries no authority by accident.

### 5A.2 The permission grammar

A permission is ASCII text of at most 256 bytes:

```abnf
permission = service "/" action "/" resource
service    = 1*16( lower / DIGIT / "-" )
action     = 1*32( lower / DIGIT / "-" )
resource   = "*" / ( 1*192( lower / DIGIT / "-" / "_" / "." / "/" / ":" ) [ "*" ] )
lower      = %x61-7A
```

Anything else is refused `permission`. The grammar is ASCII and lower case,
so no two permissions can look alike, and every implementation compares them
as bytes.

`service` names the service the permission is about: one label a service
claims for itself, registered with its application's other names in bcommon's
registry, so that two applications cannot mean different things by one
permission. `action` names something that service does. `resource` names what
it does it to, in whatever shape that service's own specification fixes: a
bucket, a channel, a namespace, a path. borg fixes the syntax and nothing
else, and a service that does not recognize an action or a resource grants
nothing for it.

`service` and `action` are never `*`, so no permission is everything
everywhere. A `resource` of `*`, or one ending in `*`, is a prefix
(section 5A.3).

Examples, each a permission some other specification defines:
`bsecret/release/payroll`, `bchat/moderate/general`, `bstore/put/photos/*`,
`bstore/get/*`.

One label is this specification's own. `borg` is registered for the
administration of an organization: defining and retiring roles, binding and
unbinding them, admitting and revoking grants, and changing groups. This
version defines no action under it, so a permission under `borg/` is well
formed and grants nothing, exactly as one for a service that does not
recognize it does. A later version that lets a key other than the
organization's sign a token (section 3.4 already has two keys signing one
chain, which is the shape) will say which `borg/` permissions such a signer
must hold for each change it makes, and a role carrying them is how an
organization delegates its administration without handing over its key.
Reserving the label now means no application claims it, and no binding made
under it today means anything it should not.

### 5A.3 Matching

A service asks whether a subject holds one permission, written out in full
with no `*` in it. A permission `h` the subject holds grants an asked
permission `a` when:

- `h` equals `a`, byte for byte; or
- `h` is `s/v/*` and `a` is `s/v/` followed by anything; or
- `h` is `s/v/p*` and `a` is `s/v/` followed by text that begins with `p`.

`service` and `action` always match exactly. Nothing else matches: there is
no suffix wildcard, no wildcard inside a resource, and no hierarchy a
separator implies. A permission asked with a `*` in it is a programming
error, and an evaluator MUST refuse to answer rather than treat the `*` as a
character or as a wildcard.

### 5A.4 Effective permissions

A subject's effective permissions in an organization are the union of what
its grant's role carries and what every binding that reaches it carries. For
a subject `K` in an organization `O`, at the reader's clock:

1. `K` holds the empty set unless it is a **current member** of `O`
   (section 13): no grant, no permissions.
2. **The grant's role.** The role named by each of `K`'s current grants
   (there may be up to 8 in one roll, section 14), where a current definition
   defines it, contributes that definition's permissions, unattenuated.
3. **Group bindings.** For each group of `O` whose canonical latest token's
   members include `K`, every current binding whose `to` is that group's
   identifier contributes its role's permissions, attenuated. Which groups
   hold `K` is settled by folding each group's own chain (section 4.3), not
   by what a host answers the `member-of` class: that answer names
   candidates, and a reader that took it as evidence would let one host hand
   a subject a role by naming a group. Leaving a group out can only take
   permissions away, so a group a reader cannot establish contributes
   nothing.
4. **Subject bindings.** Every current binding whose `to` is `K` contributes
   its role's permissions, attenuated.

A policy record is **current** when its commitment is in the policy set of
`O`'s latest token, its carrier is held (neither retracted nor superseded,
section 11.2), and, for a binding, its `expiry` is `null` or not passed,
within the 300 seconds of clock skew section 5.2 allows a grant's expiry. A
reader settles "held" the way it settles a grant's revocation: the set a
token commits to is answered whatever each carrier's status, so a record
swept since is still in that answer, and a reader that took it would grant
what the sweep has already taken away. It checks the record's funding
outpoint against a source that indexes spends, positively, as section 13
requires of a grant's, and never reads an absence as unspent. To
**attenuate** is to keep the intersection of the binding's `permissions`
with the definition's; an empty `permissions` keeps the definition's whole
set. A binding whose role no current definition defines contributes nothing.

A subject is granted an asked permission when one permission of its effective
set matches it (section 5A.3). **Nothing subtracts.** There is no deny rule,
no precedence and no order of evaluation to get wrong: a permission is held
or it is not, and the only ways to take one away are to retire the record
that carries it, to revoke the subject's grant, to remove the subject from
the group, or to let the binding expire (section 16.17).

Step 2 needs the grant's `role` field in the clear. In an organization that
is not opaque the field is revealed in the grant itself (section 5.4); in an
opaque one it is readable only to the subject, which discloses it where it
wants to use it, as BRC-52 and BRC-53 selective disclosure and the BRC-103
handshake already carry a certificate's fields. Bindings need no disclosure:
they are in the clear, so an opaque organization that wants a service to
decide without the subject's help binds roles to groups and keys rather than
naming them in grants.

### 5A.5 What no host decides

A host admits a policy record by its bytes: the grammar of its role name and
its permissions, its bounds, and the carrier rules every carrier meets
(section 11.1). It does not check that the role a binding names is defined,
that the group a binding names exists, that the subject of a binding is a
member, or that anything in the set is consistent with anything else: those
are cross-record questions, and section 11.1 says why a topic manager cannot
answer them while it admits. A binding naming a role nothing defines, or a
group that never existed, is admitted and authorizes nothing.

**No host enforces policy.** Policy is data on the plane, and the party that
enforces it is the service the permission names, which evaluates section
5A.4 for itself against its own headers and its own freshness sources. A
host answers the questions that make the evaluation cheap (section 10.2) and
nothing more. The evaluator and the signed decision receipt that records what
it decided are bcommon's (`authz`), not this contract's: this contract says
what the records mean, so that two implementations of it reach the same
verdict.

## 6. The epoch key

### 6.1 Sampling, the symmetric key and the commitment

These are BRC-369 section 2, adopted verbatim:

- the epoch key `k` is a scalar sampled uniformly from `[1, n-1]` with a
  cryptographically secure generator, fresh for each epoch, and MUST NOT be
  derived from any identity key, any earlier epoch's key or any other
  reusable secret (BRC-369 section 2.1 rule 1, section 8.1 rule 2);
- the symmetric key is `SHA-256("metanet keyed content symmetric v1" || k)`,
  `k` as 32 bytes big-endian;
- the commitment is `SHA-256("metanet keyed content commitment v1" || k)`,
  BRC-369's digest form; the point form is not used;
- the symmetric key MUST NOT be derived from the commitment (section 2.1
  rule 4).

`epoch-v1.json` reproduces BRC-369's own vector for both values and then
gives the scenario's three epochs.

The organization's tooling samples `k` for a transition that begins an epoch
when it builds the token, keeps it in its wallet's encrypted storage, and
discards it if that token is never mined (section 7.3). It MUST NOT reuse a
`k` for any other token, mined or not.

### 6.2 The release derivation

A wrap is BRC-369 section 5.2's release, with the epoch id in the content
id's place:

| BRC-43 parameter | Value |
| --- | --- |
| security level | 2 |
| protocol | `keyed content release` |
| key id | standard padded base64 of `SHA-256(epoch id \|\| epoch)`, the epoch as four bytes big-endian |
| counterparty | the member, when wrapping; the organization, when unwrapping |

The derived key id binds a wrap to one group's one epoch: a wrap cannot be
moved to another group, another epoch or another organization, because the
epoch id hashes the whole record that began the epoch. The distinct protocol
lets a wallet permit key release separately from messaging (BRC-369 section
5.2 rule 2).

### 6.3 The wrap record

Magic `"bow"` `0x01`. The payload of a wrap carrier's one output.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 6f 77 01` (`"bo"`, `'w'` for wrap, version 1) |
| 1 | `hall` | text; the hall grammar | MUST equal the topic's hall |
| 2 | `org` | bytes; the key rule | the releaser: the organization, and the carrier's owner |
| 3 | `group` | bytes(16) | the group |
| 4 | `epoch` | uint, 0 to 2^32 - 1 | the epoch whose key this releases |
| 5 | `epochId` | bytes(32) | the epoch id (section 4.1) |
| 6 | `entries` | array of 1 to 16 bytes(182) | one wrap per member, ascending by recipient key |

Keys 0 to 6 are always present; keys 7 and above are unknown to this version.
The encoded record is at most 4096 bytes. Each entry is the BRC-78
serialization of a BRC-2 ciphertext of `k`:

```text
 42 42 10 33 | org (33) | member (33) | key id bytes (32) | IV (32) | k encrypted (32) | tag (16)   = 182 bytes
```

An entry is refused (`entry`) unless it is exactly 182 bytes, begins with the
version `42 42 10 33`, names `org` as its sender, names a canonical
recipient that is neither `org` nor the anyone key, and carries the key id
section 6.2 derives from the record's `epochId` and `epoch`; entries' recipients
are strictly ascending. The ciphertext is a BRC-100 wallet's `encrypt` under
section 6.2's arguments, and the member's wallet `decrypt`s it with
counterparty `org`.

The version bytes are those BRC-78's table gives (`0x42421033`) and go-sdk
and ts-sdk write (`42 42 10 33`). BRC-78's hex example prints `10334242`, and
BRC-369's release vector carries `33104242`; a host refuses both (`epoch-v1.json` shows the BRC-369 payload,
whose ciphertext opens with its listed shared secret, refused for its header
alone).

Only the organization releases in this version: it holds every epoch key
because it sampled it, and BRC-369 section 6.2 makes the publisher its own
releaser. A wrap is never addressed to the anyone key or to anyone the
organization has not authenticated as a member (BRC-369 section 5.2 rule 4).

### 6.4 The member's checks

A member that wants an epoch's key, in this order, stopping at the first
failure:

1. `token`: it holds the group token that began the epoch (the `group-seq`
   class at the latest token's `epochSeq`), verified against its own block
   headers, and `SHA-256` of its record equals the wrap record's `epochId`;
2. `entry`: the wrap record meets section 6.3 and holds an entry whose
   recipient is the member;
3. `undecryptable`: its wallet decrypts the entry under section 6.2's
   arguments; the plaintext is exactly 32 bytes and a scalar in `[1, n-1]`;
4. `commitment`: `SHA-256("metanet keyed content commitment v1" || k)`
   equals the token's `commitment` (BRC-369 section 5.3 check 2).

Check 4 names the culprit: a key that fails it was wrapped wrong by the
organization, which signed the commitment before any member held the key. A
member keeps every epoch key it has opened in its wallet's encrypted storage,
so later reads need no host.

### 6.5 Keying content under an epoch

borg delivers the epoch key; how an application keys its own objects under it
is the application's contract. It MUST follow BRC-369 section 2.1 for every
key it uses, MUST name the organization, group and epoch of the key it used
beside each object, and SHOULD key each object with a fresh random key that
it wraps under the epoch's symmetric key, so that one object's key opens one
object (BRC-369 section 2.1 rule 2).

A writer MUST key new content under the epoch of the group's latest token,
and MUST hold positive evidence that the token is the latest, obtained within
the last 60 seconds (configurable, never more than 600):

1. the token's proof verifies against the writer's own headers; and
2. the token's 1-satoshi output is unspent according to a source that indexes
   spends (a node, a UTXO index, or a service the writer trusts). Each
   transition spends the token before it, so the unspent token is the latest
   one, and no answer at a higher `seq` from any host can contradict it.

When the source shows the output spent by a transaction not yet mined, the
writer fetches that transaction: if it is the next token and its record keeps
the same `epoch`, the writer goes on writing under that epoch; otherwise it
waits until the transaction is mined and the new epoch's key is wrapped to
it. A transition that begins an epoch therefore pauses writers for about a
block; one that only adds members does not.

Without that evidence the writer MUST NOT write: it fails closed, as BRC-169
section 4.2 rule 6 asks of a verifier with no revocation source. The age of
the check bounds how long an honest writer can key content under a
superseded epoch; a host that withholds the newer token cannot extend it,
because the unspent check does not ask that host.

A reader MUST authenticate each object's author as a member of the epoch the
object names, and SHOULD flag an object keyed under epoch `e` that it first
sees after it verified the token that began `e + 1` (allowing the writers'
freshness bound): a member removed at `e + 1` still holds `e`'s key and can
still write objects only `e`'s members read.

## 7. Membership changes

Every change is one or more mined tokens plus carriers. The organization's
tooling builds, signs, waits for the proof, and publishes in this order.

### 7.1 The flows

- **Create the organization.** Mint a chain tree and a grant tree (section
  9.4), and wait until both are mined; issue
  the first grants (carriers on the grant tree); mint organization token
  `seq` 0 spending an output of the chain tree and admitting the grants; once
  it is mined, publish it with the chain tree in its BEEF.
- **Grant.** Issue the grant carrier, funded by a grant-tree output whose
  outpoint becomes the revocation outpoint; publish it; then the next
  organization token admits its commitment. The member acquires the grant
  into its wallet (section 5.5).
- **Create a group.** Draw a group id and an epoch key; mint group token
  `seq` 0 (epoch 0, the initial members as `add`) spending a chain-tree
  output; once it is mined, publish it and the wraps of epoch 0 to every
  member.
- **Add to a group.** Mint the next token with the member in `add`. No new
  epoch is needed (the joiner reads the epoch's history, as joining a room
  means), unless the predecessor sets `rotateOnJoin`. Once it is mined,
  publish it and a wrap of the current key to the new member; with a new
  epoch, wraps of the new key to every member.
- **Add a key the group removed earlier.** The transition MUST begin a new
  epoch (section 4.2), so the returning key reads from the new epoch on and
  never what was written while it was away. The tooling MUST NOT build such a
  transition without a new epoch; the index reports one as
  `rejoin-unrotated`.
- **Remove from a group.** Mint the next token with the member in `remove`, a
  new `epoch`, a fresh key's `commitment` and `epochSeq = seq`. Every host
  refuses the token otherwise (section 4.2). Once it is mined, publish it and
  wraps of the new key to every remaining member.
- **Rotate.** A transition with no change of members and a new epoch, on a
  schedule (`rotateEvery`), or after a suspected leak of a key: rotation on
  removal gives forward exclusion, not recovery (BRC-369 section 8.4).
- **Offboard.** Revoke the member's grants (an organization token revoking
  their commitments, and a sweep of their revocation outpoints), and remove
  the member from every group that holds them, each removal with its new
  epoch.
- **Change a role.** Three changes answer to this, and which one is meant
  decides the work. To change *what a role may do*, define the role again
  with the permissions it should carry: one transition changes it for every
  holder, and nothing a member holds is reissued (section 5A.1). To change
  *who holds a role*, bind or unbind it to the group or the key: again one
  transition, and the grant is untouched. Only to change the role *named in
  a member's own grant* is the grant revoked and reissued in one
  organization token (section 5.6); no group changes. The first two are the
  ordinary case, and the reason the policy set exists.
- **A member's key changes.** The new key is a new identity: a grant to the
  new key, the old one revoked, and in every group a transition that removes
  the old key and adds the new one. Because it removes a key, it begins a new
  epoch, which is what a key change after a suspected compromise needs.

The organization MUST NOT wrap a key to anyone without a current membership
grant (section 5.6). A member whose grant expires or is revoked is owed a
removal: **rotation debt** is a group whose latest members include a key with
no current grant now (expired at the reader's clock, revoked, or not in the
roll), and every transition the organization makes in such a group first
removes those keys: the debt is removed in the same transition as any other
change, or in an earlier one, never later. The tooling SHOULD schedule each member's removal for the
moment its grant expires, so that debt does not arise. Anyone can compute the
debt from the roll and the groups (section 13), and the tooling reports and
clears it.

### 7.2 Publication order

- A token is published only after it is mined, with its proof and with the
  transaction its predecessor input spends (section 11.1), and only once the
  block holding it is in the publisher's headers and a propagation delay has
  passed (60 seconds by default), so that the hosts' headers have it too. A
  host whose headers still lack the block refuses it (`unmined`), raising, so
  nothing is recorded; the publisher confirms the token at every host it
  names by lookup and offers it again to any that lacks it.
- A wrap is published only after the token that makes its recipient a member
  of its epoch is mined in the publisher's own headers, and, where more than
  one instance of the organization's tooling can sign transitions, after one
  further block (section 7.3). A wrap of a transition that never mines would
  hand a key to someone the chain never admitted.
- A grant carrier is published before the organization token that admits
  it, so that a host holds the grant when it answers the roll.
- A sweep is published after it is mined, under the same delay as a token.

### 7.3 Concurrent changes

Two transitions built on the same tip spend the same output, and at most one
of them is ever mined: the chain is the serialization point, and no host
merges. The tooling decides that its transition took effect only when a
transaction spending the predecessor's token output is mined in its own
headers **and** that transaction's token record is byte for byte the record
it built: not when a broadcast service accepted its transaction (a service's
acceptance is not the network's, and a double spend can be accepted and then
never mined), and not by its own txid alone (a copy of its transaction with
re-encoded unlocking scripts has another txid and the same record, and is the
same transition). The losing side discards the epoch key it sampled and
anything it built on it, reads the new tip (section 7.4), and builds its
change again on top of it, rotating again if its change removes anyone. Because wraps are
published only after their token is mined (section 7.2), a losing transition
has released nothing.

When more than one instance of the tooling can sign (two devices of one
organization), a transition mined and then displaced by a reorganization in
favor of the other instance's is possible; that is why wraps then wait one
further block. A single instance never builds two transitions on one tip.

### 7.4 More than one instance: the signer key and catching up

Everything an instance of the tooling needs to go on from another
instance's change is at the hosts, except the epoch key the other instance
sampled. That key reaches a second instance the way it reaches any member:
in a wrap.

The **signer key** of an organization is the BRC-42 child of its identity
key under `[1, "borg organisation"]`, key id `signer`, counterparty anyone.
Anyone derives the public key from the organization's, so a reader can tell
that a member is the organization's own signer key; only a holder of the
organization's identity key derives the private key, and every instance of
the tooling holds that. The signer key is not the organization's key, so it
may be a member and a wrap recipient (sections 4.3 and 6.3). It is granted
and added to a group like any other key, and nothing about it is special to
a host: admission, the index and a reader's checks see one more member.

Where more than one instance can sign, the tooling SHOULD keep the signer
key a member of every group it creates or changes, holding a current grant,
so that each epoch's key is wrapped to it. An instance **catches up** by
reading as any reader does (section 13): the organization's chain and each
group's from seq 0 against its own headers, every grant of the roll (whose
root it MUST reproduce before it takes any, because a grant it lacked would
read as a member owed a removal), and each epoch's key from the wrap to
the signer key, checked against the epoch's commitment (section 6.4). It
does so after losing a concurrent change (section 7.3), whenever its spend
source shows one of its latest tokens spent, and once when it starts on a
new machine.

Two consequences follow where more than one instance can sign, and both are
part of this contract. The signer key is a member of every group, so every
roll and every group carries one member more than its people, and each
epoch's key is wrapped once more. And no instance can know what another did
since its last command, so each command first asks whether one of its
latest tokens is spent or a host lists a group it lacks, and catches up
before it builds anything; a command therefore costs those questions, and
the reads of a catch-up whenever another instance has made a change. With
one instance neither applies.

What catching up cannot bring back: the key of an epoch the signer key was
not a member of (the members of that epoch hold it; a rotation begins one
the instance holds); a grant issued and not yet admitted to the roll; and
another instance's funding trees, unpublished objects and sweeps. The epoch
key of a transition that lost is discarded by the instance that sampled it
and is needed by nobody. Two instances MUST NOT fund carriers from one
funding tree: of two carriers on one funding output a host answers one
(section 11.2), and one sweep retracts both.

## 8. What a removed member can and cannot read

When a member is removed from a group at the transition that begins epoch
`e + 1`:

**They can still read:**

- every object keyed under an epoch whose key they hold: epoch `e` and every
  earlier epoch they were a member of. Rotation is not retroactive (BRC-369
  section 8.2 rule 3);
- any object a writer keys under epoch `e` after the removal, until that
  writer sees the new epoch. An honest writer that follows section 6.5 does so
  for at most its freshness bound after the removing token is mined and its
  predecessor's output therefore spent; a writer that does not check keeps
  doing it;
- anything a current member shows them or republishes. Encryption is not
  attribution: every member of an epoch holds the same key, so a leak names
  nobody (BRC-369 section 9);
- every public fact: the hall's tokens, grants (with their revealed fields),
  the wraps' recipients, and the times of every change.

**They cannot read:**

- anything keyed under epoch `e + 1` or later. The new key is sampled
  independently (section 6.1), it is wrapped only to members of the new epoch,
  and no entry of a later wrap record names them (`wrap-v1.json`: the removed
  member opens epoch 0 and neither epoch 1 nor epoch 2). Their old key fails
  the new epoch's commitment and does not decrypt its objects;
- unless they are added again (a mined transition, visible to all, which
  begins a new epoch, so they read from that epoch on and not what was written
  while they were away, section 7.1), or a current member or the organization
  gives them the key outside the protocol, which no protocol prevents.

**They can still write** objects keyed under epoch `e`, which only epoch `e`'s
members read; readers check authorship against the epoch's members (section
6.5).

Removal takes effect when the removing token is mined and seen: before that,
the member is still a member. The window is the time to mine one transaction
plus the propagation delay and the writers' freshness bound.

## 9. Carriers, tokens, funding trees and derivations

Everything here is bcommon's, parameterized by the values in section 9.1.

### 9.1 Derivations and tags

| What | Value |
| --- | --- |
| BRC-43 protocol | `[1, "borg organisation"]` (security level 1) |
| Organization token lock and field signature | key id `org`, invoice `1-borg organisation-org` |
| Group token lock and field signature | key id `group`, invoice `1-borg organisation-group` |
| Wrap carrier record output and field signature; the outputs of the trees that fund wraps | key id `record`, invoice `1-borg organisation-record` |
| Grant carrier record output and field signature; the outputs of the trees that fund grants | key id `grant`, invoice `1-borg organisation-grant` |
| Policy carrier record output and field signature; the outputs of the trees that fund policy records | key id `policy`, invoice `1-borg organisation-policy` |
| The outputs of the trees that pay for a chain's first token | key id `chain`, invoice `1-borg organisation-chain` |
| Embedded wallet funding key | key id `fund`, counterparty `self` |
| Organization token tag | `62 6f 01` (`"bo"` `0x01`) |
| Funding-output tag | `62 6f 02` (`"bo"` `0x02`) |
| Group token tag | `62 6f 03` (`"bo"` `0x03`) |
| Epoch-key release | `[2, "keyed content release"]`, BRC-369's (section 6.2) |

Every derivation a host or a reader checks (`org`, `group`, `record`,
`grant`, `policy`, `chain`) is
BRC-42 derivation with BRC-43's `anyone` counterparty: the owner's wallet
derives with `forSelf = true`, and a reader derives the same public key from
the identity key alone (`derivation-v1.json` checks both sides). The funding
key is the exception: bcommon's embedded wallet derives it with counterparty
`self`, and no host rule reads it. The protocol name is not the application's
name because BRC-43 key derivation refuses a protocol name under five
characters: `[1, "borg"]` cannot derive (`derivation-v1.json` records the
SDK's refusal).

### 9.2 Carriers

A grant, a policy record or a wrap rides a carrier built by bcommon
`carrier.Mint`:

- **exactly one input**, spending one output of the owner's mined funding
  tree, `nSequence = 0`, unlocked by exactly one minimal push of the owner's
  signature (strict DER, low S, sighash byte `0x41`, `SIGHASH_ALL|FORKID`);
- **exactly one output**, a lock-before PushDrop of one field, the record,
  with the field signature over `SHA-256(record)`, locked to
  `derive(owner, k)`, carrying the whole value of the output it spends, where
  `k` is `grant` for a grant, `policy` for a policy record and `record`
  for a wrap (fee zero);
- **`nLockTime = 4102444800`** (2100-01-01T00:00:00Z) with a non-final input,
  so it cannot be mined. BRC-60 keeps a transaction non-final so that it can
  be updated before it is mined; here the same device is turned the other way
  round, to keep a record off the chain while SPV still verifies it through
  its funding parent.

The owner is the record's `org`: only the organization issues grants,
publishes policy and releases keys. The carrier's input script is executed against the funding
output it spends (SPV of an unproven transaction does this; a host whose
engine does not MUST do it itself), so only the owner's key could have made
it. The unlocking-script rule keeps `C` the owner's: script verification does
not require a canonical unlocking script, so without it anyone could
re-encode the input's script and present the same record under a new txid.
The owner itself can make many carriers from one funding output, since
carriers never conflict on chain; a host answers one of them per outpoint
(section 11.2), and the organization MUST NOT make more than one. A grant's
revocation outpoint is its carrier's funding outpoint (section 5), so one
grant fits one outpoint and a sweep of that outpoint revokes it. A policy
record names no outpoint of its own, and a sweep of its carrier's funding
outpoint retires it (section 9.5).

### 9.3 Chain tokens

An organization or group token is a mined transaction with exactly one output
that claims a token, a lock-before PushDrop of two fields and the field
signature, holding 1 satoshi (its other outputs, such as change, claim
nothing):

```text
<derive(org, "org")>   OP_CHECKSIG <62 6f 01> <O> <signature> OP_2DROP OP_DROP
<derive(org, "group")> OP_CHECKSIG <62 6f 03> <G> <signature> OP_2DROP OP_DROP
```

The signature is the wallet's strict low-S DER signature over
`SHA-256(field 1 || field 2)` under the locking key. A token at `seq > 0`
MUST spend the token at `seq - 1` of the same chain; a token at `seq` 0 MUST
spend at least one output of the organization's chain tree, locked to
`derive(org, "chain")` (section 9.4). Only the organization's key can do either:
the token is mined, so its inputs' scripts were valid, and the output it
spends is locked to the organization's derived key. Copying a token's
signed output script into another transaction makes nothing a host admits,
because that transaction cannot spend the predecessor. Two tokens spending
one predecessor are a double spend of which at most one is ever mined. A
token's other inputs pay its fee from any source and are not read. The
organization MUST NOT spend a token's output except in its successor: a token
output spent by anything else ends the chain, and a reader that finds the
latest token spent by a transaction that is not a token reports the chain
**terminated** (section 13).

A token is published with the transaction its predecessor input (or, at
`seq` 0, its funding input) spends, each with its own Merkle path, in one
BEEF (section 11.1), so that a host reads the predecessor's record from the
bytes it was given and never from what it holds. The token's own unlocking
scripts are not held to a canonical form: a re-encoded copy of a token
transaction mined in its place has the same outputs and the same record, so
it is the same transition, but its txid differs, so a successor built on the
original txid never mines and is rebuilt (section 7.3).

The record rides in the token itself, as the state, because a reader then
needs only the token's transaction and a block header to know the state, and
the record is small (section 14).

### 9.4 Funding trees and publication

A funding tree is a mined transaction of equal outputs, each a PushDrop of
the one field `62 6f 02` with no signature, locked to `derive(owner, k)`:
`<key> OP_CHECKSIG <62 6f 02> OP_DROP`, where `k` is the key id of what the
tree pays for: `grant`, `policy`, `record` (wraps) or `chain`. A carrier
spends only a **mined** tree's output, and its BEEF carries that tree with
its proof, so a carrier's ancestry is one mined parent. A publisher never
publishes a tree to the hall on its own: it travels inside the BEEF of every
carrier it funds, and of a chain's first token. A host given a mined tree on
its own admits it as a sweep (section 11.1), which retracts nothing. bcommon `producer` runs a tree's
lifecycle.

The four kinds of tree are kept apart by their keys, so hosts enforce the
separation (section 11.1), and each sits in its own wallet basket: **grant
trees** (key `grant`, basket `borg grant funding`), whose every output funds
at most one grant and is spent again only to revoke it; **policy trees** (key
`policy`, basket `borg policy funding`), whose every output funds at most one
policy record and is spent again only to retire it; **record trees**
(key `record`, basket `borg record funding`), which fund wraps; and **chain
trees** (key `chain`, basket `borg chain funding`), whose outputs fund no
carrier and pay for chains' first tokens. A chain tree is small (8 outputs
by default), because a chain's first token carries it whole. A mined spend of a grant tree's output revokes the grant it funded, so a sweep
of a whole grant tree is a revocation of every grant on it: a kill switch
that sweeps every tree (bcommon `producer` sweeps all of a home's trees) is,
for an organization, the revocation of its whole roll, and the tooling says
so before it runs.

### 9.5 Retraction and revocation

A **sweep** is a mined transaction spending funding outputs, used and unused
(bcommon `carrier.Sweep`, whose output 0 is a funding-shaped tombstone, which
is what makes a host admit it). Once it is mined, every carrier that spent
one of those outputs is a double spend and can never be valid again: hosts
retract it, a grant among them is revoked for every BRC-52 verifier, and a
policy record among them is retired, so the role it defined or the
permissions it bound are gone from the moment the sweep is mined and seen.
Retracting the carrier is the fast half of a withdrawal; the organization's
next token, which drops the commitment from the policy root, is the
committed half, and a reader that checks the root sees both. A
sweep is published once, after it mines (section 7.2), to the plane and
directly to every host the publisher names. Hosts learn of spends only from
sweeps; only a mined spend retracts. Retraction reaches honest hosts only: it
is not erasure, and a copy anyone kept stays a copy.

## 10. Halls and lookup

### 10.1 Hall identifiers and topics

A hall's topic is `tm_borg_<name>_<suffix>`, and its **hall identifier** is
`<name>_<suffix>`:

- `<name>` is the readable part: 1 to 31 bytes of lowercase ASCII letters and
  single underscores, starting and ending with a letter;
- `<suffix>` is exactly 10 lowercase ASCII letters, each drawn uniformly at
  random from `a` to `z` by whoever creates the hall, once, when it creates
  it, and never changed.

The identifier is 12 to 42 bytes and splits at its last underscore. The whole
topic is a BRC-87 topic manager name of at most 50 characters. The
`tm_borg_` namespace belongs to borg. The identifier, never the readable part,
is what every record's `hall`, every question and every topic carries; a
renderer that shows the readable part SHOULD show the suffix beside it.

The object plane is shared: anyone may publish to any topic, and readable
names collide. A suffix drawn at random once per hall makes an accidental
collision negligible (about 2^-47 for any one pair). It is not a secret and
not an access control. An organization picks a hall its members' hosts
carry, or creates one; a host advertises the halls it carries by BRC-88 SHIP
like any other topic.

### 10.2 The lookup service `ls_borg`

One lookup service per host answers for every hall the host carries. It
answers BRC-24 questions of exactly these classes:

| Class | Question (`query` object) | Answer (`output-list`) | Price |
| --- | --- | --- | --- |
| `org` | `{"hall": h, "org": o}` | the organization's latest tokens: those at the highest `seq` held, at most 8 | free |
| `org-seq` | `{"hall": h, "org": o, "seq": s}` | the organization's tokens at `seq` `s`, at most 8 | free |
| `roll` | `{"hall": h, "roll": o}` | the grant carriers whose commitments the roll of `o`'s latest token holds, first page | free |
| `roll-after` | `{"hall": h, "roll": o, "after": c}` | the same, after the commitment `c` | free |
| `member` | `{"hall": h, "org": o, "member": k}` | the grant carriers in that roll whose subject is `k`, at most 8, each with its inclusion path in the roll as context | free |
| `policy` | `{"hall": h, "policy": o}` | the policy carriers in the policy set of `o`'s latest token, first page | free |
| `policy-after` | `{"hall": h, "policy": o, "after": c}` | the same, after the commitment `c` | free |
| `role` | `{"hall": h, "org": o, "role": r}` | the policy carriers in that set whose `role` is `r`, definitions and bindings alike, first page, each with its inclusion path in the policy root as context | free |
| `role-after` | `{"hall": h, "org": o, "role": r, "after": c}` | the same, after the commitment `c` | free |
| `bound` | `{"hall": h, "org": o, "boundTo": x}` | the bindings in that set whose `to` is the group identifier or identity key `x`, first page, each with its inclusion path as context | free |
| `bound-after` | `{"hall": h, "org": o, "boundTo": x, "after": c}` | the same, after the commitment `c` | free |
| `groups` | `{"hall": h, "groupsOf": o}` | the latest token of each of `o`'s groups, first page | free |
| `groups-after` | `{"hall": h, "groupsOf": o, "after": g}` | the same, after the group id `g` | free |
| `member-of` | `{"hall": h, "org": o, "memberOf": k}` | the latest token of each group of `o` whose members include `k`, first page | free |
| `member-of-after` | `{"hall": h, "org": o, "memberOf": k, "after": g}` | the same, after the group id `g` | free |
| `group` | `{"hall": h, "org": o, "group": g}` | the group's latest tokens, at most 8 | free |
| `group-seq` | `{"hall": h, "org": o, "group": g, "seq": s}` | the group's tokens at `seq` `s`, at most 8 | free |
| `wraps` | `{"hall": h, "org": o, "group": g, "epoch": e}` | the wrap carriers of epoch `e`, first page | free |
| `wraps-after` | `{"hall": h, "org": o, "group": g, "epoch": e, "after": c}` | the same, after the commitment `c` | free |
| `wrap` | `{"hall": h, "org": o, "group": g, "epoch": e, "member": k}` | the wrap carriers of epoch `e` with an entry for `k`, at most 8 | free |
| `sweep` | `{"hall": h, "spent": p}` | the admitted mined transactions that spend the outpoint `p`, at most 8 | free |
| `history` | `{"hall": h, "history": o, "chain": x}` | every token of a chain of `o`, `x` being `"org"` or a group id, first page | priceable |
| `history-after` | `{"hall": h, "history": o, "chain": x, "after": s}` | the same, after `seq` `s` | priceable |

The query is validated as parsed, so hosts in any language reach the same
verdict. A member given twice takes its last value, as JSON parsers do. Every
member is a string: `hall` meets the hall grammar; `org`, `roll`, `member`,
`groupsOf`, `memberOf` and `policy` are 66 lowercase hex digits whose bytes
meet the key rule, as is `history`; `group` is 32 lowercase hex digits;
`role` is a well-formed role name (section 5A.1); `boundTo` is 32 lowercase
hex digits (a group identifier) or 66 whose bytes meet the key rule (an
identity key); `seq` is a
decimal from 0 to 2^53 - 1 and `epoch` from 0 to 2^32 - 1, with no leading
zero; `chain` is `"org"` or a group id; `spent` is BRC-100's `OutpointString`
(`<txid>.<index>`, txid in display order, index in decimal with no leading
zero); `after` is a txid in display order for `roll-after`,
`wraps-after`, `policy-after`, `role-after` and `bound-after`, a group id for
`groups-after` and `member-of-after`, and a `seq` for `history-after`. A question MUST carry exactly the members of one class. A
missing member, a member no class defines, or a value that breaks these rules
is refused with an error, never answered empty and never ignored. A question
that names a hall the host does not carry is refused with an error too.

**Latest.** A chain's latest tokens are those at the highest `seq` the host
holds. A host that can check a token's proof against its current headers
leaves out one whose proof no longer verifies; a host that cannot (section
11.6) answers every candidate. A reader always chooses among them itself
(section 13).

**Order and pages.** Grant and wrap carriers are in ascending order of txid
(display order, as lowercase hex strings), groups by group id, tokens by
`seq` then txid. A page holds at most 64 outputs; the `-after` classes answer
only what sorts strictly after the cursor. A `history` or `history-after`
page never splits a `seq`: it ends before a `seq` whose tokens do not all
fit (a first `seq` is answered whole, up to its 8), so the cursor, a `seq`,
loses nothing.

**Inclusion paths.** Each output of a `member` answer carries, as its BRC-24
`context`, the canonical CBOR array `[seq, index, size, path]`: the `seq` of
the organization token whose roll the answer used, the grant's leaf index in
that roll, the roll's size, and the RFC 6962 inclusion path as an array of
`[bytes(32), bool]` steps from the leaf upward, the bool `true` when the
sibling is on the left (bcommon `commit.Prove`). A reader verifies the path
from the grant's commitment to that token's `roll` (bcommon `commit.Verify`),
so one membership is checked without reading the roll; the leaf and node
prefixes of RFC 6962 keep a path from passing a subtree off as a leaf, so
`index` and `size` are hints. A context is at most 2048 bytes and 32 steps.
A context is a hint: a path that does not verify is ignored, and the reader
falls back to the roll. `org-v1.json` carries one.

The `role` and `bound` answers carry a context of the same shape, whose
`seq` is the organization token whose **policy** root the answer used and
whose path runs from the policy record's commitment to that token's `policy`
(section 3.3). A reader that holds the latest token checks one role or one
binding without fetching the set; `policy-v1.json` carries one of each.

An empty answer means only that this host holds nothing that answers: a
reader MUST NOT read it as proof that something does not exist. The engine
hydrates each answered output as BEEF, so a reader verifies a token or a
carrier with nothing but its own block headers.

### 10.3 Free and priceable

A price attaches to a question class, never to part of an answer. The
twenty-one free classes are answered at no price, without authentication, on
every conforming host. No priced class is a superset of a free class: the
priceable classes carry `history` and `chain`, which no free class carries,
and lack `org`, so no spelling turns a free question into a priced one; and
an unknown member is refused, never ignored. Everything a reader needs to check
a membership, a group's state, an epoch key, a role, a binding or a
revocation is free, because a service that cannot decide for free cannot be
asked to decide at all; the priceable classes sell the bulk walk of a chain, which the free `org-seq` and
`group-seq` give one token at a time.

A priced question is asked on the same BRC-24 `/lookup` route over BRC-104
authentication; the host answers 402 with BRC-105's headers and answers the
question once paid. This contract fixes what BRC-105 leaves open: the payment
is output 0 of the transaction in BRC-105's `x-bsv-payment` header, a P2PKH
output of at least the price to the key BRC-29 derives for the payee
(protocol `[2, "3241645161d8"]`, the asker as counterparty, as ts-sdk's
`AuthFetch` builds it). A host that prices anything publishes its
terms at `GET <base>/ls_borg/terms`, where `<base>` is the origin of
`ls_borg` (BRC-180 rule 2: a service defines its own routes), as JSON
`{"service": "ls_borg", "terms": 1, "classes": [{"class": c, "satoshis": n}]}`.
The document is informative; the 402's headers are what a client pays
against, and a host that serves no terms (404) charges for nothing. No price
is ever in a record.

The payment is the payer's own transaction, and a host broadcasts nothing
(section 11.1). The payer therefore broadcasts its own payment once the host
has accepted it, so that it mines whether or not the payee has settled; the
payee broadcasts it again when it settles, which is harmless. Until it is
mined the payer could spend the same coins elsewhere, which is the payee's
risk and the reason a payee settles promptly.

## 11. The host

### 11.1 Admission by `tm_borg_<name>_<suffix>`

**What a verdict may depend on.** An admission verdict depends only on the
bytes of the submission (the transaction and the transactions its BEEF
carries, each bound to it by its txid) and the host's block headers. It never
depends on another topic, on anything this topic holds, or on the order
objects arrived in. This is deliberate. A topic manager that reads another
topic, or a policy record, while it admits is not stable on the reference
engine: it has been measured to refuse a transaction whose dependency was
submitted at the same time (every time, for a pair), in the same transaction
(always), or synchronized in the other order (always, silently, and never
repaired), because the engine checks every topic before it writes any and
holds no lock across submissions. And the engine hands a topic manager only
the indices of the inputs whose outputs the topic holds (its previous coins,
BRC-22), never those outputs. So a chain token carries its predecessor's
transaction in its own BEEF, and the topic manager reads the predecessor's
record from those bytes; every cross-record question (is this member's grant
current? is this the latest epoch? did the organization wrap to everyone?) is
a status the lookup service computes and a reader checks (sections 11.2 and
13). The previous coins decide only which held outputs an accepted
transaction retains. SPV of the BEEF is the engine's and runs before the
topic manager, including the script of an unproven transaction's inputs
against the outputs they spend. Checks then run in the order given; a
transaction is refused for the first rule it breaks, with that rule's reason.

**Admit or raise.** An overlay engine records every transaction its topic
manager returns admittance instructions for, empty ones included, and treats
a later submission of the same txid as a duplicate no topic manager sees
again. A recorded refusal would let anyone suppress a genuine object by
submitting it first in a different BEEF, would keep a token refused for a
block the host's headers did not yet have refused forever, and would keep a
host that upgrades from admitting what it refused before. So a topic manager
returns admittance instructions only for a transaction it admits (at least
one output admitted), and **raises for every refusal**; the engine then
stores nothing.

**The BEEF.** Every refusal of a submission's BEEF carries the one label
`beef`: a BEEF over the host's bound (at least 262144 bytes); one that does
not parse as exactly one BEEF V1 (BRC-62), BEEF V2 (BRC-96) or Atomic BEEF
(BRC-95) with nothing after it; one without its subject transaction; one that
holds anything but what the object needs:

- a **carrier**: exactly two transactions, the carrier and the funding tree
  its input spends, and exactly one Merkle path, the tree's;
- a **chain token**: exactly two transactions, the token and the transaction
  that its predecessor input (at `seq` 0, its funding input) spends, and
  exactly two Merkle paths, one for each, or one when both are in one block
  (below). This is a BEEF V1 or V2, never an
  Atomic BEEF, which by BRC-95 holds only a subject's unproven ancestry;
- a **sweep**: exactly one transaction and its own Merkle path.

Every path is minimal: at its lowest level exactly the proven txid, flagged
as the txid, and its sibling (a hash, or a duplicate flag); above that exactly
one leaf a level. When a token and its parent are in one block, BEEF writers
merge their paths, so the BEEF holds exactly one path, the union of the two
minimal paths, flagging exactly the two txids and holding no other leaf. A merged path MAY
omit a sibling that is itself an ancestor of the other txid (the node the
other path computes at that level, or at the lowest level the other txid
itself, which is flagged); nothing else may be omitted, and nothing else may
appear. Two separate paths for a token and a parent mined in one block are
refused (`beef`): the BEEF writers merge them, so two of one block are not
what a writer builds. The organization's tooling SHOULD still build a token
only on a parent mined in an earlier block. Every transaction is carried whole: a txid-only entry (BRC-96)
is refused. The counts are those the BEEF declares on the wire, before any
parser merges or collapses anything.

Every script comparison below is byte for byte against the script rebuilt
from the decoded fields and the derived key, exactly as bcommon `pushdrop`
writes it: the 33-byte compressed key, `OP_CHECKSIG`, each field as one
minimal push, the signature as one minimal push when the output is signed,
and the fewest `OP_2DROP` then `OP_DROP` that clear the fields.

**Classify.** An output's locking script is read only as far as this: it
starts with `0x21`, 33 bytes and `0xac`, then one push (an opcode `0x01` to
`0x4b`, or `OP_PUSHDATA1`, `2` or `4` with its little-endian length, every
declared byte present). The output **claims a grant** when that push's data
starts with a CBOR definite-length map head, `00 44` and `62 6f 6d 01`, **a
policy record** with `62 6f 70 01`, **a wrap** with `62 6f 77 01`, **an organization token** when the data is exactly
`62 6f 01`, **a group token** when it is exactly `62 6f 03`, and it is
**funding-shaped** when the whole script is `0x21 <33 bytes> 0xac 03 62 6f 02
75` (`script-v1.json`). A transaction with more than one claiming output, or
with a claiming output of a carrier kind and one of a token kind, is refused
(`shape`).

**Grant, policy or wrap carrier.** A transaction whose one claiming output
claims a grant, a policy record or a wrap. The owner is the record's `org`:

1. `shape`: it does not have exactly one input and exactly one output, or the
   output's value differs from the value of the output the input spends (read
   from the funding tree in the BEEF);
2. `beef`: the BEEF is not exactly the carrier and its funding tree with the
   tree's minimal path;
3. the record's own rules (section 3.1), with their reasons;
4. `hall`: the record's `hall` is not this topic's;
5. `mineable`: `nLockTime < 4102444800`, or the input's `nSequence` is
   `0xFFFFFFFF`;
6. `unlock`: the input's unlocking script is not exactly one minimal push of a
   strict low-S DER signature followed by the sighash byte `0x41` (bcommon
   `carrier.CheckUnlocking`);
7. `lock`: the output script is not the well-formed one-field signed PushDrop
   locked to `derive(owner, k)`, `k` being `grant` for a grant, `policy` for a
   policy record and `record` for a wrap;
8. `signature`: the field signature is not strict low-S DER, or does not
   verify over `SHA-256(record)` under that key;
9. `funding`: the output the input spends, read from the funding tree in the
   BEEF, is not a well-formed funding output locked to `derive(owner, k)`,
   the same `k` as rule 7;
10. for a grant: `grant-revocation` when the record's `revocation` is not the
    input's outpoint; `grant-signature` when the certificate's signature does
    not verify (section 5.1); `grant-reveal` when the record carries `reveal`
    and a key does not open its field or a value breaks its rule. A wrap's
    entries, and a policy record's role name, permissions and binding shape,
    were checked with the record (steps 6 and 7 of section 3.1); a policy
    record has no rule of its own here, and nothing about what it means is
    checked (section 5A.5).

Rules 5 to 8 follow bcommon `carrier.Validate`'s order, stricter in rule 8.
Rules 6 and 9, with the engine's execution of the input's script, make the
commitment the owner's alone.

**Organization or group token.** A transaction whose one claiming output
claims a token. Its **parent** is the other transaction in its BEEF:

1. `shape`: the claiming output is not a PushDrop of exactly two fields and a
   signature, or does not hold 1 satoshi;
2. `beef`: the BEEF is not exactly the token and its parent, each with its
   minimal path;
3. the record's own rules, with their reasons;
4. `hall`: not this topic's;
5. `lock`: the output script is not the well-formed two-field signed PushDrop
   locked to `derive(org, "org")` for an organization token or
   `derive(org, "group")` for a group token;
6. `signature`: the field signature is not strict low-S DER, or does not
   verify;
7. `unmined`: the token's or the parent's proof does not verify against the
   host's headers;
8. `token-input`: for `seq > 0`, the parent's txid is not `prev`, or no input
   of the token spends an output of the parent that is a well-formed token of
   the same kind and hall, of the same organization (its `org` equal to this
   record's, or naming it as its `successor`, section 3.4) and of the same
   group, at `seq - 1`, or more than
   one input spends a token output of the parent (an output that classifies
   as a token, well formed or not); for `seq` 0, no input spends
   an output of the parent that is a well-formed funding output locked to
   `derive(org, "chain")`, or an input spends a token output of the parent;
9. the transition rules against the predecessor (the parent's token output):
   section 4.2 for a group token (`transition`, `rotation`, `count`), and for
   an organization token `transition` (same hall, the next `seq`, `prev`
   naming the predecessor's transaction, the same `opaque`, an `org` equal to
   the predecessor's or named by its `successor`, section 3.4) and `count`
   (the predecessor's `count` plus `admit` less `revoke`, and the
   predecessor's `policyCount` plus `define` less `retire`).

A **well-formed** token output, here and wherever a predecessor is read, is
one that classifies as a token of its kind (its tag), is a PushDrop of
exactly two fields and a signature, holds 1 satoshi, decodes as a record of
its kind, equals byte for byte the script rebuilt from its fields and the
key derived from the record's `org` (canonical), and whose field signature
is strict low-S DER and verifies under that key. The predecessor's record is
not held to its own transition rules: those were its own admission's.

A token's inputs that do not spend the parent are not read: they pay its fee.
A token is admitted whether or not the topic already holds its predecessor;
the index then reports it `pending` until it does (section 11.2), so a token
that arrives before its predecessor is not lost.

**Sweep.** A transaction whose outputs claim nothing, whose output 0 is
funding-shaped (under any key: a sweep's owner is whoever could sign its
inputs), and whose proof verifies against the host's headers: output 0 alone
is admitted, and its inputs' outpoints are matched against the funding
outpoints of carriers. A sweep is refused in the order `unmined` (its proof
does not verify against the host's headers), `beef` (the BEEF is not
exactly the sweep and its own minimal path), `too-large` (below). A mined
funding tree published on its own has this shape (output 0 funding-shaped,
nothing claimed) and is admitted as a sweep: that is harmless, because its
inputs spend the publisher's wallet outputs, which are no carrier's funding
outpoint, and it holds one output at the host. A publisher still never
publishes a tree on its own (section 9.4). Only output
0 is admitted, so a mined transaction cannot load a host with held outputs,
and nothing unmined is admitted, so a stranger cannot fill a topic with free
ones. A host MAY refuse (`too-large`) a sweep of more than its input bound,
which is at least 1024; such a sweep is about 150 KB on the plane, so an
organization sweeps large trees in parts on the plane. A host keeps a spend
for its life only when the outpoint is a carrier's funding outpoint; spends
of other outpoints it keeps in a bounded table (host policy, at least 65536
outpoints, oldest dropped first), so that a sweep that arrives before the
carriers it retracts still retracts them, while a stranger's chain of sweeps
costs the host only that table.

Anything else is refused (`not-borg`), raising like every refusal. In
particular a transaction that only spends outputs the topic holds is not
accepted: it would let anyone store unmined bytes at every host for nothing.

**Retention and spends.** For every transaction it accepts, the topic manager
lists every input that spends an output the topic holds as a coin to retain,
so no admitted output is deleted when something spends it: a token stays
answerable after the next one spends it. The lookup service records, for each
carrier, the funding outpoint its input spends, and for each admitted sweep,
the outpoints its inputs spend (above); it does not rely on the engine's spend
notices, which cover only held outputs.

**What a host does not do.** It does not evict unproven carriers: carriers
are unproven by design. It does not broadcast anything: an engine that
carries `tm_borg_` topics runs with no broadcaster, since a broadcaster is
set per engine, not per topic. It counts refusals by reason. A host SHOULD
keep a token or sweep it refused `unmined` for a short while and decide it
again once its headers move, since a mined object on the plane is delivered
once.

### 11.2 The index

The lookup service keeps, per hall and organization: the tokens of each
chain by `seq` (up to 8 per `seq`); the grants by commitment and by subject;
the policy records by commitment, by role and by what they are bound to; the
wraps by group, epoch and recipient; and one **outpoint row** per funding
outpoint any admitted carrier spends, holding the commitments of the carriers
on it, which of them is the outpoint's **winner** (the lowest `C`, compared
as 32 bytes in hash byte order), whether the winner was dropped, and the
admitted sweeps that spend the outpoint. A carrier's status is a function of
the admitted set, the outpoint rows and the host's headers, never of arrival
order (the first that applies):

| Status | When |
| --- | --- |
| `retracted` | an admitted sweep, in the host's best chain, spends the carrier's funding outpoint. For a grant this is revocation by spend |
| `superseded` | it is not its outpoint's winner, or its outpoint's winner has been dropped |
| `held` | otherwise |

The `roll` and `member` classes answer every grant carrier whose commitment
the roll holds, and the `policy`, `role` and `bound` classes every policy
carrier whose commitment the policy set holds, whatever its status, so that a
reader verifying the root sees each and checks its status itself. Otherwise a held carrier is answered
and a retracted or superseded one is not. Up to 8 superseded wrap carriers
per outpoint are kept as evidence that the owner equivocated; the rest are
counted and MUST be dropped. Superseded grant and policy carriers are kept, up to 64 per
outpoint, until the outpoint is swept, because a later organization token may
name any of them in its roll or its policy set; beyond 64 the organization has equivocated at a
scale no host follows.

The index folds each chain from `seq` 0 along the spends it holds and records
per token: `pending` while its predecessor is not held (a token admitted out
of order, or met first on restore); `roll-mismatch`, `policy-mismatch` or
`members-mismatch` when the fold breaks (sections 3.2, 3.3 and 4.3);
`commitment-reuse` when a group
token's new epoch repeats an earlier epoch's commitment; and
`rejoin-unrotated` when a group token adds a key an earlier token of the
group removed without beginning an epoch (section 4.2). A token gets the
first that applies, in this order: `members-mismatch` (and
`roll-mismatch`, then `policy-mismatch`, for an organization) is sticky, so
every later token of the chain has it too; then `commitment-reuse`; then `rejoin-unrotated`; else the
token is consistent. `pending` is not a fold result but the host's state
while it lacks a predecessor, and a reader that holds the whole chain never
sees it. The answers of
`roll`, `member`, `member-of`, `policy`, `role` and `bound` use the fold of
the highest token whose fold is complete (no `pending` token below it), so a reader that compares the
answer with the latest token sees the gap; with several such candidates the host uses the one that verifies in its
headers, or, if it cannot tell, the first-mined candidate (lower height, then
lower position in its block), and a reader decides.

### 11.3 Retention

How long a host keeps what it no longer answers is its policy, with floors:

- tokens for the host's life: they are the chain, a few hundred bytes each;
- outpoint rows, and the sweeps they record, for the host's life: without the
  row, a carrier made later on a swept outpoint would be a fresh txid and
  would be answered. Spends of outpoints no carrier has used are kept in the
  bounded table of section 11.1 only;
- a grant while the roll holds it, and at least 31 days after it leaves the
  roll or is retracted;
- a policy record while the policy set holds it, and at least 31 days after
  it leaves the set or is retracted: a service that has to answer for a
  decision it made needs the record the decision rested on;
- the wraps of each group's current epoch, and those of an earlier epoch at
  least 31 days after the host first saw the token that ended it. A member
  keeps the keys it opened (section 6.4); a longer retention is what a host
  sells, never a floor.

To **drop** a carrier is to delete its outputs from the topic and mark its
outpoint row; the engine's record that the transaction was applied remains,
so a dropped carrier offered again is a duplicate.

### 11.4 Restore

The engine does not replay admissions. On start the service rebuilds its
index from the topic's stored outputs and the outpoints it persisted, and
folds every chain from `seq` 0, whatever order storage returns them in; a
token met before its predecessor is `pending` until the fold reaches it.

### 11.5 Reorganizations

A host learns of a block only through its headers, and an application on the
reference host is given no signal when a block leaves the best chain. So a
host may hold two tokens at one `seq` (the one mined first, orphaned, and the
one mined in its place), may keep a token whose stored proof names an orphaned
block although the same transaction was mined again elsewhere, and may keep
counting as spent an outpoint whose sweep was orphaned. It answers every
candidate, up to 8, and keeps the outpoint rows as they are. A token or a
sweep a host holds is held with its proof, and a submission of the same
txid with a new proof is a duplicate: the reference engine takes no proof by
submission for a transaction it holds proven, and refreshes proofs only
through its own proof provider, for transactions it has already demoted. A
transaction the host holds unproven is upgraded when it is submitted again
with a proof; borg holds no such transaction (carriers are never mined, and
tokens and sweeps are admitted only mined). Readers
decide with their own headers (section 13): a token or a sweep whose proof
does not verify against the reader's best chain is not mined for that reader,
who fetches the transaction's current proof from a chain source by its txid. Because an epoch id
hashes the record, not the txid, a transition that is mined again after a
reorganization, as it usually is, keeps its epoch id, and its wraps stay
valid.

### 11.6 Joining a hall

A host that starts carrying a hall, or that missed part of a chain, is given
the chain by replay, from the publisher or from anyone who reads a sibling
host. A host stores a mined token as the token alone with its proof (the
engine keeps an Atomic BEEF of a proven transaction and drops what else came
with it), so a sibling's answer is not a submission a host admits as it
stands. A replayer assembles each token's BEEF itself: the token from the
`org-seq` or `group-seq` answer at `seq`, and its parent from the answer at
`seq - 1`; at `seq` 0 the parent is the chain tree, which no host keeps, so
it comes from the publisher or from a chain source by its txid, with its
proof. A chain source may drop a transaction once all its outputs are
spent, and a chain tree is fully spent once each of its outputs paid for a
chain's first token, so the publisher's own record of the tree is what
lasts. Which of the two a replayer asks first is its own choice; either way
the tree is held to the same checks: its txid is the one the token's input
names, its proof verifies against the replayer's headers, and the assembled
BEEF passes section 11.1. Tokens, carriers and sweeps are self-verifying
and may be replayed in any order; replaying in `seq` order only spares the host its `pending`
interval.

A host MUST NOT enable an unordered synchronization of unspent outputs for
`tm_borg_` topics: one carries only unspent outputs, so it never carries a
chain's spent tokens, and it drops what a topic manager refuses without
retrying it.

## 12. Delivery: plane and unicast

A publisher (the organization's tooling) reaches the hall's hosts in one of
two ways. The records, the hosts and the readers are the same in both.

| | plane | unicast |
| --- | --- | --- |
| The publisher submits each object | once, to its facade on the plane; mined objects also directly to every host it names | to every host it names, each on its own |
| Every other host receives it | from the plane, which repairs loss | only from the publisher, or from anyone who copies it across later |
| A host that was down | receives what it missed from the plane's repair, within the plane's window; anything older by replay (section 11.6) | misses it until someone submits it again |
| Needs | a plane that carries the hall's topic | nothing but the hosts |

**Plane.** The publisher submits each object once, with the hall's topic
first in its topic list; the plane delivers it to every subscribed host and
repairs loss. Nothing in borg disables, bypasses or asks for an exception
from repair, for any class, hall or configuration; lateness is the reader's
to handle. Tokens and sweeps are also submitted directly to every host the
publisher names, because a host off the plane learns of the chain only by
receiving it, and a host whose headers lagged refused it (`unmined`) and does
not receive it from the plane again. They are published only after the
propagation delay of section 7.2, which makes that refusal rare. A publisher
that names hosts SHOULD also look for each wrap at each of them once the
facade took it (the `wrap` class answers a wrap as soon as a host holds it),
and offer it directly to a host that still lacks it after a few seconds:
that is a copy across (below), not a way round the plane's repair, and a
direct copy that meets the plane's is a duplicate. A grant is confirmed the
same way once the organization token that admits it is published.

**Unicast.** The publisher submits the same bytes to each host it names (at
most 16), retries each on its own, and counts the object published once a
quorum of hosts took it. A host **took** an object when its answer admits it,
or when a lookup at that host answers it: to a submitter a duplicate and a
raised refusal look the same, so an answer that admits nothing is confirmed by
lookup before it counts. Tokens and sweeps must reach every host named. A
grant carrier is the exception to confirming at once: no free class answers
a grant before the roll admits it, so a grant is counted when sent, then
confirmed by the `member` class at each host once the organization token
that admits it is published, and offered again to any host whose answer
lacks it.

**In order.** Chain tokens are mined one after another, so they are naturally
published a block or more apart. A host admits a token whose predecessor it
does not hold and reports it `pending` (section 11.2), so lateness or
reordering on the plane loses nothing; the publisher still confirms each
token at each host it names by lookup and offers it again to any that lacks
it.

**Filling a host that missed something.** Tokens, carriers and sweeps are
self-verifying, so anyone who holds one may submit it to a host that lacks
it: a member that finds its wrap at one host and not another copies it
across; anyone who finds a host behind on a chain replays the missing tokens
to it, assembling each token's BEEF as section 11.6 says.

**Reading.** A reader asks one or more hosts the same question and verifies
every answer against its own headers (section 13). A reader that asks several
and receives different sets reports the difference as such, never merges it
silently.

## 13. Verification by a reader

A reader checks everything against its own block headers and fails closed.

**The organization's state.** Ask `org` (one or more hosts). Among the
answered tokens, keep those whose proof verifies against the reader's best
chain and that meet section 11.1's token rules; the **canonical** chain is
the one that runs by spends from the first-mined `seq` 0 token (lower height,
then lower position in its block). Its highest token is the organization's
latest. A reader that holds an earlier latest token checks that the new one
descends from it (`org-seq` back to it), so that a host cannot answer an old
state as the latest. Where the chain changes its `org` the reader checks that
the predecessor named that key as its `successor` (section 3.4); the
organization's current key is the latest token's `org`, and its key history
is the sequence of `org` values along the chain. Whether a token is the latest is settled the way a
writer settles it (section 6.5): its 1-satoshi output is unspent according
to a source that indexes spends. A reader without such a source trusts the
highest `seq` any asked host answers, asks more than one, and says that its
answer may be stale.

**Is `K` a current member?** Ask `member` for `K` at the organization's
latest token; verify each answered grant carrier (the carrier rules and
section 5's content rules) and its inclusion path against the token's `roll`
(section 10.2). Without a path that verifies, ask `roll` (all pages), verify
every grant, and compute the roll root of their commitments: a mismatch means
the host withheld or added a grant, so ask another host. Then `K` is a
current member when a grant in the roll has subject `K`, a scope that grants
`member` (section 5.3), an expiry not passed, a reveal unless the
organization is opaque, and an unspent revocation outpoint. "Unspent" is a
positive answer, never an absence: a node or UTXO index the reader trusts
says the outpoint is unspent, mempool included, or at least two sources
agree, each checked within 60 seconds for anything that moves value or
releases a key (BRC-169 section 4.2 rules 2, 4 and 6). A spend is found by
the `sweep` class at one or more hosts or by a chain source, and counts only
when its proof verifies. A reader with no source that can say "unspent" does
not treat the grant as current for such an action; for display it MAY show
the member, marked unverified.

A chain whose latest token's output is spent by a transaction that is not a
token is **terminated**: the reader shows it so and keys nothing under it.

**A group's state.** Ask `group`; choose the canonical latest token as for
the organization. Its `members` root is the group's membership. To enumerate
the members, ask `wraps` for the latest token's epoch (all pages), verify each
wrap carrier, and take the recipients of every entry; their members root MUST
equal the token's `members`. A recipient not in the set is a **stray** wrap,
which the organization made to someone the chain never admitted, and a member
of the set with no entry has an **incomplete** release: both are reported,
and the group is shown as inconsistent. An incomplete release is reported
**pending**, not inconsistent, while the reader's best chain is at most one
block past the latest token's block: wraps are published only after their
token mines and, where several instances of the tooling can sign, one
further block (section 7.2). A stray wrap is never pending.

**An epoch key.** Section 6.4. A reader that holds the commitments of earlier
epochs checks that the new commitment repeats none of them, and a reader that
folds the group's chain checks that no token re-adds a removed key without
beginning an epoch (section 4.2).

**A removal.** The token at `seq` `s` lists `K` in `remove` and begins epoch
`e + 1` (any admitted token does, by section 4.2); `K` is in the members root
of no later token (the enumeration above, at the latest token); and no wrap
of an epoch after `e` names `K`. The last is the absence of an answer, so it
is evidence and not proof; what makes the removal cryptographic is that the
key of epoch `e + 1` is fresh and wrapped only to the members the root names.

**What may `K` do?** Ask for the policy records that could carry the
permission in question: `role` for the role `K`'s grant names, `bound` for
`K`, and `bound` for each group the `member-of` answer gives. Verify each
answered carrier and its inclusion path against the latest token's `policy`
root; without a path that verifies, ask `policy` (all pages), verify every
record and compute the root, and a mismatch means the host withheld or added
one. Ask another host, or assemble the verified records of several hosts by
commitment and hold the assembled set to the same root: a set that
reproduces it is the committed set whichever hosts its records came from,
because nothing can be added to the root or left out of it and still match,
and each record was verified on its own before it was counted. Assembly
widens what a reader can read, not what it trusts, which was never a host's
word. A reader that assembles names every host it drew from. Then compute
the effective set (section 5A.4) and match the asked permission (section
5A.3).

Every input of that answer is checked against the reader's own headers, so a
service decides from the chain and not from a host's word. It **fails
closed**: a service that cannot settle which token is the latest, or cannot
say that the subject's grant is current, denies. Nothing is cached as an
allow beyond the freshness the service itself states (section 16.18), and the
asked permission carries no `*`. bcommon `authz` is this procedure, and
writes the signed receipt that says what it decided and over which `seq`.

**Rotation debt.** A group of the organization whose latest members include a
key that is not a current member of the organization now (the two checks
above). Anyone can compute it;
the organization's tooling clears it (section 7.1).

**A host's answer is not authority.** A host can withhold a token, a grant or
a wrap; it cannot forge or alter one. The roots make withholding from the roll
or a membership detectable from one host; comparing hosts makes withholding a
newer token detectable.

## 14. Size and rate bounds

| Quantity | Bound | Why |
| --- | --- | --- |
| Organization record, encoded | 8192 bytes | 64 admitted and 64 revoked commitments take about 4.2 KB |
| Group record, encoded | 8192 bytes | 64 added and 64 removed keys take about 4.3 KB |
| Grant record, encoded | 4096 bytes | a revealed grant is about 850 bytes |
| Policy record, encoded | 8192 bytes | 24 permissions of full length take about 6.1 KB |
| Permissions per policy record | 24 | bounds one record and one role's reach; a role that needs more is several roles, or one prefix |
| Permission, encoded | 256 bytes | the grammar's longest permission is 243 bytes (section 5A.2) |
| Policy records in a set | 65536 | bounds the set's fold, as the roll's bound does |
| Role name | 64 bytes | the size of the grant's `role` field, which names it |
| Wrap record, encoded | 4096 bytes | 16 entries take about 3.1 KB |
| Changes per transition (`admit`, `revoke`, `define`, `retire`, `add`, `remove`), each | 64 | bounds one token and the index work it causes; a larger change is several transitions |
| Grants in a roll | 65536 | bounds a roll's fold |
| Members of a group | 1024 | BRC-369 section 8.3: the linear release serves groups of a few hundred |
| Entries per wrap record | 16 | a carrier of 16 entries is about 6 packets |
| Inputs of a sweep a host accepts | at least 1024 | a sweep of a 1000-output tree |
| Grants for one subject in a roll | 8 | the `member` answer's limit |
| Entries in a record map | 64 | bounds the work of one record |
| `scope` / `role` / display names | 512 / 64 / 64 bytes | |
| Answers | 8 tokens, carriers or sweeps per question; 64 per page (section 10.2) | bounds an answer; not a floor, a later version may raise it |
| Superseded carriers kept per funding outpoint | 8 wraps, 64 grants | evidence of equivocation; a later roll may name any grant; the rest are counted |
| Spends of outpoints no carrier used, kept by a host | at least 65536, oldest dropped first | a sweep may arrive before the carriers it retracts |
| Inclusion context | 2048 bytes, 32 steps | a roll of 65536 grants needs 16 |
| BEEF | exactly what the object needs (section 11.1); a host's bound is at least 262144 bytes | |
| Object on the plane | the plane operator's object bound (BRC-149) | a publisher checks every object against it before publishing |

A reader accepts every record within these bounds. A later version may raise a
bound, never lower one, so a record published under a bound stays readable.
Below them, what a publisher sends and how a host limits its readers are
policy: [limits.md](limits.md) sets the recommended defaults and caps,
measured. A host MAY rate-limit lookups, answering 429 with `Retry-After`, and
a client MUST respect it.

## 15. Alignment with existing patterns

| Choice | Pattern followed | Where it differs, and why |
| --- | --- | --- |
| Members hold certificates issued by the organization; authority in a certificate, never in shared keys | BRC-169 section 9 (delegation: "Authority travels in a certificate, never in shared key material"; section 9.8: an organization "issues each member a delegation certificate and reassigns access by revoke-and-reissue") | a type of its own (section 5.1) with `role`, `scope` and `expiry`; no `delegator` handle, because the organization is named by its key |
| The grant's shape, signature, field encryption and keyring | BRC-52 (certifier, subject, serial, revocation outpoint, encrypted fields, master keyring, `[2, "certificate signature"]`, `[2, "certificate field encryption"]`) | laid out as CBOR rather than BRC-52's binary form; the binary form is rebuilt to verify. Unless opaque, a public reveal: the field revelation keys in the clear, which reveals to everyone what a BRC-52 keyring reveals to one verifier |
| Certificate type id | BRC-169 section 4.5's derivation (`type = base64(SHA-256(derivation string))`, versioned by the string); BRC-52 (one type, one set of field names and meanings) | the derivation string is borg's |
| Scope grammar and fail-closed reading | BRC-169 section 9.2 | `*` refused; a qualifier this version does not define denies its verb |
| Revocation by outpoint spend; revocation status from an index, not SPV; expiry against the reader's clock | BRC-52 ("If that UTXO has been spent, the certificate MUST be treated as revoked"); BRC-169 sections 4.2, 9.6 | the revocation outpoint is the grant carrier's funding outpoint, so one sweep both revokes for every verifier and retracts the carrier at every host; the roll root is a second, batched revocation |
| Certificate custody | BRC-169 section 4.6; BRC-100 `acquireCertificate` | none |
| Epoch key sampling, symmetric key, commitment | BRC-369 sections 2.1 and 2.2 (digest form) | adopted verbatim; the point form and split keys (section 6.4) are not used |
| Epochs, rotation on removal, `previous` link, linear release | BRC-369 section 8 ("A room MUST begin a new epoch whenever a participant ceases to satisfy its gate"; "linear in the number of participants") | the epoch descriptor is the group record of the token that begins the epoch, in CBOR, signed by the token's field signature under the organization's derived key rather than JSON signed by the identity key (a BRC-100 wallet cannot sign with its identity key); its id is SHA-256 of the record; `previous` is the chain's `prev` and `epochSeq`; rotation on removal is enforced by every host, not only by the releaser |
| The release and its derivation | BRC-369 section 5.2 (`[2, "keyed content release"]`, key id `SHA-256(content id \|\| epoch)`); BRC-78 serialization; BRC-2 symmetric form; BRC-42 and BRC-43 | the wraps ride the plane in a record of up to 16, not one message per recipient to a messagebox; version bytes as both SDKs write them, not as BRC-369's vector writes them |
| Checking a released key | BRC-369 section 5.3 (check 2, the commitment) | check 1 (ciphertext hash) and 3 (the tag) belong to each application's content |
| Releaser | BRC-369 section 6.2 (the publisher as its own releaser) | delegated releasers (section 6.1) are not in this version |
| Group key agreement | BRC-369 section 11 ("Tree-based rekeying ... needs a group key agreement specification"); RFC 9420 | linear, as BRC-369 section 8.3; the group chain settles which change applies at each step, the ordering RFC 9420 leaves to its delivery service, without a server |
| Ordered membership on a chain of mined tokens; a fork is a double spend | BRC-48 Pay to Push Drop tokens spent forward; BRC-22 topic managers retaining previous coins | no coordinator: each group and each organization is its own serialization point; the predecessor is read from the token's BEEF, never from what a host holds |
| Records never mined, kept off chain | BRC-60's non-final transaction, which it keeps open to update before mining | turned the other way round: a far-future lock time keeps the record off the chain for good |
| Canonical CBOR records with integer keys; unknown keys preserved | RFC 8949 section 4.2.1 for the encoding; BRC-174 section 3.2 for preserving unrecognized fields | one codec with every application on the same library |
| Record magic: prefix, type letter, version | BRC-171 section 6's header of a protocol prefix, a version and a record type | in the order bcommon's registry fixes (prefix, type letter, version) |
| Roll and membership roots | RFC 6962 Merkle Tree Hash | none |
| Topic and lookup names | BRC-87; BRC-22; BRC-24 with `output-list` answers | a random suffix per hall |
| Host discovery | BRC-88 SHIP | a chain is joined by replay, each token's BEEF assembled by the replayer, not by an unordered synchronization (section 11.6) |
| Priced questions | BRC-105 over BRC-103 and BRC-104; BRC-29; BRC-180 rule 2 | only the bulk history classes are priceable; the payment's output index and script are fixed here, where BRC-105 leaves them to the implementation |
| Carrier ancestry and proofs | BRC-62, BRC-96 and BRC-95 BEEF; BRC-74 BUMP | exactly what the object needs, so nobody can attach anything to it; a chain token carries its proven parent in a BEEF V1 or V2, which Atomic BEEF cannot |
| Authority named in a record the organization's chain commits to, evaluated by the service that enforces it | BRC-52 and BRC-169 section 9 (authority in a certificate, scoped, expiring and revocable); BRC-100's permission tokens, a wallet's record of what an application may ask of it | the corpus binds authority to a certificate the subject holds, so changing it reissues certificates; here the certificate names a role and the chain carries what the role may do, so authority changes in one transition and every decision can name the `seq` it rested on |
| Permission as `service` / `action` / `resource` with a prefix wildcard | the shape policy languages settle on | borg fixes the syntax and leaves each service its own actions and resources; there are no deny rules, no precedence and no inheritance, so an evaluator has nothing to get wrong (section 5A.4) |
| Roles bound to groups, not only to subjects | relationship-based authorization (Zanzibar and its successors) rather than classic role-based access control | the relationship is a group chain whose membership is already ordered, certified and revocable, so a binding needs no store of its own and a removal already rotates the key |
| A binding attenuates its role | BRC-169 section 9.2's scope, read fail-closed | the attenuation is the intersection with the definition, so a binding can never widen a role |
| Key succession announced one token ahead | no BRC fixes a mechanism; the common shape is a successor announced under the old key and claimed under the new | both keys sign one transaction, because only the predecessor can spend the announcing token's output and only the successor can sign the claiming record, and a host checks the handover from two transactions' bytes |
| Objects on the plane | BRC-148 and BRC-149 | none |

The BRC-52 certificate verifier and the BRC-369 release code (wrapping an
epoch key, opening a wrap and checking an entry) are borg's own (Go
`borgrec`, TypeScript `host/src`). What a second application
shares is bcommon's, unchanged in behavior and checked against the same
vectors: the bounded record reader (`record`), the BEEF a chain token, a
carrier and a sweep are admitted in (`chaintoken`), and in Go the BRC-369
content key, its symmetric key, its commitment and the check on an opened
key (`keyed`). borg pins bcommon v0.23.0 in both languages.

## 16. What this does not do

1. **Membership is public metadata.** Who belongs to which group, when they
   joined and left, how many members a group has and how often it rotates are
   visible to every host in the hall and to anyone who follows its topic: the
   chain lists every addition and removal, and every wrap names its
   recipient. A non-opaque organization also publishes roles and scopes. The
   opaque setting hides roles and scopes, not membership: a grant names its
   subject, and a group names its members.
2. **No forward secrecy for wraps.** A wrap is BRC-2 under BRC-42 derivation
   from long-term identity keys. Whoever later learns a member's identity key
   decrypts every wrap ever addressed to it that any host or copy still holds,
   and with them every epoch that member was in. Rotation protects the future
   from a departed member, not the past from a compromised key.
3. **No recovery without rotation.** A leaked epoch key exposes that epoch
   until the group rotates; rotation on removal is not proactive recovery
   (BRC-369 section 8.4). A group that wants recovery on a schedule sets
   `rotateEvery` and the organization's tooling acts on it; no host enforces it.
4. **Rotation is linear.** A removal from a group of `n` costs `n - 1` wraps,
   in records of up to 16; groups above a few hundred members wait for a
   tree-based construction.
5. **Leaks name nobody.** Every member of an epoch holds the same key.
6. **The organization's key is everything.** Whoever holds it can grant,
   remove, re-add and read every epoch. A compromised organization key is a new
   organization; this version has no succession.
7. **Revocation is as fresh as the index asked.** SPV cannot prove an output
   unspent; a reader learns that an output is unspent only from a node or an
   index, with its lag and honesty, and a reader with no such source cannot
   tell a revoked member from a current one for anything that matters. Hosts
   learn of revocations only from sweeps.
8. **Removal takes effect when mined and seen**, not when decided (section 8).
9. **Hosts can withhold** a token, a grant or a wrap; they cannot forge or
   alter one. Roots make withholding from the roll or a membership visible;
   asking several hosts makes a withheld newer token visible.
10. **Expiry depends on the reader's clock**, within 300 seconds of skew.
11. **Retraction and revocation are not erasure.** Honest hosts stop answering
    and may drop; a copy anyone kept stays a copy, and a key a member opened
    stays opened.
12. **A writer's freshness check is visible.** A writer that asks a third
    party whether a group's latest token is unspent, every minute, tells it
    which groups it writes to and when. A writer's own node, or a batched
    query, keeps that to itself.
13. **A removed member can still write** under the epochs whose keys they
    hold; readers that check authorship against an epoch's members (section
    6.5) are the defense.
14. **A token's transaction can be re-encoded by a miner.** A token's
    unlocking scripts are not held to one form, so a miner can mine a copy
    with another txid. The transition is the same, but the organization must
    rebuild its next transition on the copy's txid.
15. **Succession does not carry custody.** A handover moves the chain, the
    roll, the policy set and the groups to the successor's key, and it
    rotates every group's epoch (section 3.4). It does not move the carriers
    the predecessor issued or the funding trees whose outputs revoke and
    retire them: until the successor reissues them on its own trees, every
    revocation and every retirement still needs the predecessor's key. A key
    that is lost or compromised before it announces a successor cannot hand
    over at all.
16. **Policy is public.** Roles, their permissions and every binding are in
    the clear on the hall, so anyone following the topic learns an
    organization's structure: which roles exist, what they may do, and which
    groups and keys hold them. The opaque setting hides a grant's fields, not
    the policy set.
17. **No deny rules.** A permission is held or it is not; nothing subtracts,
    and there is no precedence to reason about. So a binding published by
    mistake stays effective until it is retired, its subject's grant is
    revoked, its subject leaves the group, or it expires, and each of those
    takes a mined transaction that hosts and services have seen.
18. **A decision is as fresh as what the service asked.** Permissions rest on
    an unspent revocation outpoint and on which token is the latest, and
    neither is provable by SPV (point 7). A service that caches a decision
    owns that staleness; the receipt it writes names the `seq` it used, which
    is what makes the staleness auditable rather than invisible.
19. **Policy is not a capability.** A role is not a bearer token: nothing in
    a grant or a binding lets a party prove offline what it may do, because
    what a role may do is in the organization's chain and can change without
    touching the grant. A service with no path to a host and to its headers
    cannot decide, and denies.

## 17. Vectors

`testdata/vectors` holds the golden vectors, generated by the codec in
`borgrec` with `GOWORK=off go run ./cmd/vectors`; `go test`
regenerates them and compares byte for byte, refuses a file nothing
generates, and reads the hex, refusal and query files back without the
generator. The generator fails when a check it makes stops holding: a
member's wallet opens its wrap and its grant, another member's does not,
every certificate verifies, BRC-369's own vector still reproduces, the SDK
still refuses a four-character protocol name, and every transaction case is
admitted or refused by exactly the rule it names. The TypeScript codec in
`host/` runs every one of them and reaches the same verdict on each, and
writes `testdata/ts/ts-v1.json`, which the Go tests read: records, grants
issued and wraps sealed by the TypeScript SDK, and carriers and tokens it
built, each decoded, opened or admitted by the Go side (`make verify` runs
both directions).

| File | What it holds |
| --- | --- |
| `derivation-v1.json` | the registry values; the test identities; the keys each derives for `org`, `group`, `record`, `grant`, `policy` and `chain` under counterparty anyone, checked against the owner's own wallet; the SDK's refusal of `[1, "borg"]`; the grant type; halls with one readable part and two suffixes, and one suffix drawn from fixed bytes |
| `epoch-v1.json` | BRC-369's vector reproduced (symmetric key, commitment, release key id and invoice, the release payload opened with its listed shared secret, and its version bytes, which borg refuses); the scenario's three epoch keys with their symmetric keys, commitments, epoch ids and release key ids |
| `org-v1.json`, `org-*.hex` | the organization's genesis (three grants), the offboarding (one revoked) and an opaque organization's genesis, with their roll roots; the transition admitted, refusals of each transition rule, and a `member` answer's inclusion context that verifies for one grant and not for another; a genesis whose policy set is empty, and a handover: a token announcing a `successor` and the next token carrying that key, refused against a predecessor that names nobody and refused for a key the predecessor did not name |
| `group-v1.json`, `group-*.hex` | a group's create, add, remove and scheduled rotation, with members after each, epoch ids and commitments; every transition admitted, refusals of each transition rule against its predecessor, a join with `rotateOnJoin` admitted with its new epoch and refused without it, tokens whose fold breaks (`members-mismatch`), and a removed key re-added with and without a new epoch (`rejoin-unrotated`) |
| `grant-v1.json`, `grant-*.hex` | four grants (three revealed, one opaque) with their values in the clear, the stand-in revelation keys and IVs, the ciphertexts, the keyring, the BRC-52 binary form signed and the signature; each member opens its own and another member cannot |
| `wrap-v1.json`, `wrap-*.hex` | the four wrap records of the scenario with each entry's IV, and what each member's wallet opens from each, with the check label of each failure: the removed member opens epoch 0 and neither epoch 1 nor epoch 2; a wrap held against another epoch's token fails check 1 |
| `scope-v1.json` | the scope grammar and its fail-closed meaning case by case; expiry and role cases |
| `policy-v1.json`, `policy-*.hex` | eight policy records with their commitments: two role definitions, a second definition of one of them, a binding to a group, two to a key (one attenuated, one with an expiry), one whose role nothing defines and one whose expiry has passed. A chain of two organization tokens carries them: the first defines five, the second defines two and retires one, each with the set and the root after it. One inclusion context of the shape a `role` or `bound` answer carries, which verifies against the policy root of its `seq` and against the earlier root not at all. The two definitions of one role, read as the intersection of their permissions. The records' own refusals are `refusal-v1.json`'s, and what each subject holds is `authz-v1.json`'s |
| `permission-v1.json` | the permission grammar accepted and refused case by case, the longest permission the grammar allows, and the matching rule of section 5A.3 over exact, `*` and prefix cases, including a prefix that must not match and an asked permission carrying `*`, which is refused |
| `authz-v1.json` | the effective-permission procedure of section 5A.4 over the policy set of `policy-v1.json`, one case to a rule: a subject whose grant names a defined role, the same key holding only through its group, one whose grant names a role nothing defines and whose own binding is attenuated, one reached only by a binding whose role nothing defines and one whose expiry has passed, one whose role field is not a well-formed role name, and one who is no member at all. Each with the set it holds and three asked permissions, granted or denied |
| `refusal-v1.json` | byte strings each record's rules refuse, each with its reason (section 3.1), and the grant's content rules; a record with an unknown key, preserved |
| `query-v1.json` | every class with its members, price and limit; questions, each with its class or refused |
| `script-v1.json` | the classifier of section 11.1 over real locking scripts from bcommon's PushDrop, and scripts that claim nothing |
| `transaction-v1.json` | transactions as BEEF, each with the previous coins it is admitted with, admitted or refused by the rules of section 11.1 in their order: grant and wrap carriers, organization and group tokens (one mined in its parent's block, so its BEEF holds one merged path, and one as BEEF V2), a sweep, and a mined funding tree published alone; refusals of every carrier rule (shape, BEEF, record, hall, mineable, unlock, lock, signature, funding, grant content), every token rule (shape, BEEF, record, hall, lock, signature, unmined, token input, transitions), a role definition and a binding admitted on their own tree, and a policy record refused for its funding key, for its locking key and for a role name the grammar refuses, sweeps and transactions that are not borg's. Every input of every case passes the script interpreter, so each refusal is made by its rule alone; a few cases break two rules and are refused for the earlier. `stored` holds each admitted token as the engine stores it, read back without its parent, and the BEEF a replayer assembles from it (section 11.6), byte for byte the one first admitted |
| `index-v1.json` | the index's rules of section 11.2: the chains of `transaction-v1.json` folded, chains whose fold breaks (`roll-mismatch`, `policy-mismatch`, `members-mismatch`, `commitment-reuse`, `rejoin-unrotated`), and funding-outpoint rows with each carrier's status |

The vectors' hall is `example_hall_vkqzmtrwxp`; its suffix is a fixed example
so that the vectors are reproducible. The test identities' private keys are 32
bytes of one value (`0x51` the organization, `0x52` to `0x54` the members,
`0x55` a second, opaque organization, `0x56` a key that is never a member);
they are public and never for real use. Every value a client draws at random
(epoch keys, field revelation keys, IVs, serial numbers, the group id) is a
fixed stand-in, `SHA-256("borg/vector/<label>")`. The successor identity's
private key is 32 bytes of `0x57`. The record vectors use
stand-ins of the same form for the commitments and txids of grant carriers
and mined tokens; `transaction-v1.json` builds real ones on a test chain whose
headers it lists. Signatures are deterministic (RFC 6979).
