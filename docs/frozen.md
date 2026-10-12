# Frozen at first publish

Every item below is hashed into a key, written into a record, or named by a
host. Once one record using it is on a public host, readers verify against it
for as long as anyone reads that record, so changing it afterwards is a new
identifier or a new record version, never an edit.

**Status: the rows that carried the organization roll, the groups, the
grants and the wraps are signed. The rows added for the policy record
(section 5A), the organization record's policy keys, the succession rules of
both chain records, and the six policy lookup classes are PROVISIONAL and
await signature; they are marked so in each table. Nothing of this contract
has been released, so the policy keys were added to the records as they
stand rather than as a second record version.**

**Status of the signed rows: signed, every one.** Key id `signer` (section 1) was signed with
the two consequences spec sections 7.4 and 10 state: where more than one
instance can sign, the signer key is a member of every group and each
command first catches up; and a payer broadcasts its own payment for a
priced question. The four rows of section 2 that waited
for the transaction vectors (carrier shape, chain-token shape, script
canonicality and sweep) and the carrier-input half of the signatures row
were signed against the transaction vectors (95 at signing; 100 since the
policy cases were added), with the wording of spec
sections 11.1, 11.2 and 11.5 that the vectors settled. The registry rows of section 1
belong in bcommon's registry (`docs/registry.md`), where they are checked
against every registered row; this repository does not edit that registry.

## 1. Registry entries

Checked against the registered prefixes (`bf`, `gw`, `bl`, `bb`, and `vx`
for tests) and the prefixes held for planned applications (`bs`, `bc`): `bo`
is none of them, `[1, "borg organisation"]`
is no registered protocol, `tm_borg_` and `ls_borg` are no registered name,
and no basket starts with `borg`.

| Kind | Value | Use | Status |
| --- | --- | --- | --- |
| Protocol | `[1, "borg organisation"]` | every borg derivation. BRC-43 refuses a protocol name under five characters, so `[1, "borg"]` cannot derive (`derivation-v1.json` records the SDK's refusal) | signed |
| Key id | `org` | an organization token's lock and field signature | signed |
| Key id | `group` | a group token's lock and field signature | signed |
| Key id | `record` | a wrap carrier's record output and field signature, and the outputs of trees that fund wraps | signed |
| Key id | `grant` | a grant carrier's record output and field signature, and the outputs of trees that fund grants | signed |
| Key id | `policy` | a policy carrier's record output and field signature, and the outputs of trees that fund policy records | provisional |
| Key id | `chain` | the outputs of trees that pay for a chain's first token; never a carrier's | signed |
| Key id | `fund` | the embedded wallet's funding key | signed |
| Key id | `signer` | the organization's signer key: a member key every instance of the organization's tooling derives, which a wrap is addressed to like any member's (spec section 7.4). No host or reader rule reads it | signed |
| Tag prefix | `bo` (`62 6f`) | owned by borg | signed |
| Tag | `bo` `0x01` | organization token, first field | signed |
| Tag | `bo` `0x02` | funding-tree output, its one field | signed |
| Tag | `bo` `0x03` | group token, first field | signed |
| Record magic | `boo` `0x01` | organization record, version 1 | signed |
| Record magic | `bog` `0x01` | group record, version 1 | signed |
| Record magic | `bom` `0x01` | membership grant record, version 1 | signed |
| Record magic | `bop` `0x01` | policy record, version 1 | provisional |
| Record magic | `bow` `0x01` | wrap record, version 1 | signed |
| Topics | `tm_borg_<name>_<suffix>` | one topic per hall: a readable `<name>` of at most 31 characters and a 10-letter random `<suffix>` fixed at the hall's creation (spec section 10.1); the `tm_borg_` namespace belongs to borg | signed |
| Lookup service | `ls_borg` | one per host, all halls; every question names its hall | signed |
| Host route | `<base>/ls_borg/terms` | the host terms document (spec section 10.3) | signed |
| Baskets | `borg fund`, `borg grant funding`, `borg policy funding` (provisional), `borg record funding`, `borg chain funding`, `borg chain`, `borg kill tombstone` | wallet baskets: grant trees, policy trees, record (wrap) trees and chain trees kept apart (spec section 9.4), chain tokens, tombstones; never on a public host, registered so no two applications share one in a wallet | signed |
| Certificate type | `base64(SHA-256("borg membership grant v1"))` = `poEEbYGwpJbTzKTeSRBJzV31s8PNknNycAbyjDV3ALc=` | the BRC-52 type of a membership grant, by BRC-169 section 4.5's rule. The registry does not list certificate types today; proposed as a new row kind | signed |
| Borrowed, not registered | `[2, "keyed content release"]`, key id `SHA-256(epoch id \|\| epoch)` | BRC-369's release derivation, used as written; any BRC-369 implementation shares the protocol, and the key id separates every epoch | signed (as a note in the registry) |

Tag type bytes `0x04` to `0x66`, `0x68` to `0x6c`, `0x6e`, `0x71` to `0x76`
and `0x78` to `0xff` under `bo`, and record letters other than `o`, `g`, `m`,
`p` and `w`, are unassigned and stay borg's. `0x67`, `0x6d`, `0x6f`, `0x70`
and `0x77` are never assigned as tag type bytes, because `bo 0x67`,
`bo 0x6d`, `bo 0x6f`, `bo 0x70` and `bo 0x77` are the first three bytes of
the record magics. `0x00` is not assigned. A record version is in the record
magic's fourth byte and never in a tag, so the organization and group tokens
of both versions carry the tags `bo 0x01` and `bo 0x03`.

## 2. Mechanism shared with the substrate

| Item | Value | Status |
| --- | --- | --- |
| Derivation setting | BRC-42, counterparty `anyone`, `forSelf = true` on the owner's side | signed |
| PushDrop layout | lock-before, field signature last, over `SHA-256` of the fields concatenated | signed |
| Carrier non-final device | writers use `nLockTime = 4102444800` and input `nSequence = 0`; hosts accept any `nLockTime >= 4102444800` and any `nSequence` below `0xFFFFFFFF` | signed |
| Carrier shape | exactly one input spending an owner funding output of a mined tree, unlocked by exactly one minimal push of a strict low-S DER signature and sighash `0x41`; exactly one output carrying the whole input value, locked and funded under `grant` for a grant and `record` for a wrap; the owner is the record's `org`; the BEEF is exactly the carrier and its funding tree with one minimal Merkle path; refused in the order `shape`, `beef`, record rules, `hall`, `mineable`, `unlock`, `lock`, `signature`, `funding`, content rules (spec section 11.1) | signed |
| Chain token shape | exactly one claiming output, a two-field signed PushDrop of 1 satoshi; mined; published in a BEEF V1 or V2 of exactly the token and its parent (the transaction its predecessor input, or at `seq` 0 its funding input, spends), each with its minimal path (one merged path flagging exactly both txids when they share a block, which may omit a sibling that is itself an ancestor of the other txid and holds nothing else; two separate paths of one block refused `beef`; no txid-only entries); `seq > 0`: the parent's txid is `prev` and exactly one input spends the parent's token at `seq - 1`, whose record the host reads from the parent; `seq` 0: an input spends a funding output of the parent locked to `derive(org, "chain")` and none spends a token; the organization spends a token output only in its successor; other inputs not read and not held to a canonical form; admitted whether or not the topic holds the predecessor (`pending` until it does); refused in the order `shape`, `beef`, record rules, `hall`, `lock`, `signature`, `unmined`, `token-input`, transition rules; the predecessor is a well-formed token output (its tag, two fields and a signature, 1 satoshi, canonical, its signature verifying) | signed |
| Script canonicality | every checked script equals, byte for byte, the one rebuilt from its fields and the derived key | signed |
| Signatures | field, carrier input, certificate signatures strict DER, low S; carrier input signatures sighash `0x41` only | signed |
| Classifier | the key push, `OP_CHECKSIG`, then one push: a CBOR definite map head, `00 44` and `bom 0x01` claims a grant, `bop 0x01` a policy record (provisional), `bow 0x01` a wrap; the push exactly `bo 0x01` an organization token, `bo 0x03` a group token; the whole script `21 <33> ac 03 62 6f 02 75` is funding-shaped; two claiming outputs refuse the transaction (`script-v1.json`) | signed |
| Funding output | `<derive(owner, k)> OP_CHECKSIG <62 6f 02> OP_DROP`, unsigned, `k` being `grant`, `record` or `chain` by what the tree pays for | signed |
| Sweep | a mined transaction claiming nothing whose output 0 is funding-shaped: output 0 alone admitted; its inputs' spends kept for the host's life where they match a carrier's funding outpoint, otherwise in a bounded table of at least 65536; refused in the order `unmined`, `beef`, `too-large`; a host accepts at least 1024 inputs. A mined funding tree published on its own has this shape and is admitted as a sweep, which is harmless (it spends no carrier's funding outpoint); publishers never publish one. A transaction that only spends held outputs is refused (`not-borg`) | signed |
| Retraction | an admitted sweep spending a carrier's funding outpoint retracts the carrier; for a grant it is revocation, and an organization revokes only by sweep; for a policy record it is retirement (provisional) | signed |
| Publication | tokens and sweeps published once, after they mine and a propagation delay, to the plane and directly to every host named, then confirmed by lookup and offered again where missing; a wrap only after the token that makes its recipient a member is mined (one further block when several tooling instances can sign); a grant before the token that admits it; funding trees only inside the BEEF of what they fund | signed |
| Commitment | `C = txid(K)`, hash byte order in records, display order in JSON | signed |
| CBOR | RFC 8949 section 4.2.1 core deterministic encoding, bcommon's subset, enforced on decode | signed |
| Admission | a verdict depends only on the submission's bytes (the transaction and the transactions its BEEF carries) and the host's headers; never on another topic, on what the topic holds, or on arrival order; previous coins decide only retention; **a topic manager returns instructions only for what it admits and raises for every refusal** | signed |
| One answered carrier per funding outpoint, over the host's life | the winner is the lowest `C` (hash byte order) on the outpoint; every other carrier is `superseded`, and so is every carrier once the winner is dropped; up to 8 wraps or 64 grants kept as evidence (grants until the outpoint is swept) | signed |
| Outpoint rows | one per funding outpoint any admitted carrier spent (commitments, winner, dropped, sweeps), kept for the host's life | signed |
| One carrier per funding outpoint, by the organization | an organization makes at most one carrier per funding output; `roll` and `member` answer every grant whose commitment the roll holds, whatever its status | signed |
| Hosts broadcast nothing | an engine that carries `tm_borg_` topics runs with no broadcaster | signed |
| Coin retention | every accepted transaction retains every held output it spends | signed |
| Joining a hall | by replay, the replayer assembling each token's BEEF from the token and its parent (`seq - 1` answer, or at `seq` 0 the chain tree from the publisher or a chain source); an unordered synchronization of unspent outputs is never enabled for `tm_borg_` topics | signed |
| Index statuses | carriers `retracted`, `superseded`, `held`; tokens `pending`, `roll-mismatch`, `members-mismatch`, `commitment-reuse`, `rejoin-unrotated` | signed |

## 3. The organization record (magic `boo` `0x01`)

| Item | Status |
| --- | --- |
| Keys: 0 `magic`, 1 `hall`, 2 `org`, 3 `seq`, 4 `prev`, 5 `name`, 6 `admit`, 7 `revoke`, 8 `count`, 9 `roll`, 10 `opaque`, 11 `created`; all required | signed |
| Types and bounds of spec section 3; `prev` zero exactly at `seq` 0; the first token admits its whole roll and revokes nothing | signed |
| The roll: before less `revoke` plus `admit`; its root RFC 6962 over the grant commitments ascending, the empty root `SHA-256()` | signed |
| Transition rules: same hall and organization, `seq + 1`, `prev` the predecessor's txid, the same `opaque` (fixed at the first token), `count` = predecessor's + `admit` - `revoke` | signed |
| The organization key is never the anyone key, in any record | signed |
| Keys 12 `define`, 13 `retire`, 14 `policyCount`, 15 `policy`, 16 `successor`; all required, keys 17 and above unknown | provisional |
| The policy set: before less `retire` plus `define`; its root RFC 6962 over the policy commitments ascending, the empty root `SHA-256()` | provisional |
| Succession: a token's `org` equals its predecessor's or the key the predecessor named as `successor`; the handover token is locked to the successor's derived key and spends the announcing token's output, so both keys sign it. A `successor` is a well-formed key, never the anyone key and never the record's own `org` | provisional |
| Transition rules: `policyCount` = the predecessor's plus `define` less `retire`; a first token defines its whole policy set and retires nothing | provisional |

## 4. The group record (magic `bog` `0x01`)

| Item | Status |
| --- | --- |
| Keys: 0 `magic`, 1 `hall`, 2 `org`, 3 `group`, 4 `seq`, 5 `prev`, 6 `name`, 7 `epoch`, 8 `epochSeq`, 9 `commitment`, 10 `add`, 11 `remove`, 12 `count`, 13 `members`, 14 `rotateOnJoin`, 15 `rotateEvery`, 16 `created`; all required | signed |
| Types and bounds of spec section 4; the group id 16 random bytes; neither the organization nor the anyone key a member; the first token is epoch 0 at `epochSeq` 0, adds its whole membership and removes nothing | signed |
| **Rotation is enforced at admission:** a transition with any removal, or with an addition after a predecessor that sets `rotateOnJoin`, begins the next epoch; a new epoch has a commitment other than the predecessor's and `epochSeq = seq`; otherwise `epoch`, `commitment` and `epochSeq` are the predecessor's | signed |
| **Re-adding a removed key rotates:** a transition that adds a key an earlier token of the group removed MUST begin a new epoch; the tooling MUST rotate, and the index reports a token that does not as `rejoin-unrotated` (not an admission rule: the predecessor names no removed keys) | signed |
| Members: before less `remove` plus `add`; the root RFC 6962 over `SHA-256(key)`, keys ascending | signed |
| The epoch id: SHA-256 of the encoded group record of the token that began the epoch | signed |
| Key 17 `successor`, required, keys 18 and above unknown; the succession rules of the organization record, applied to a group chain, so that group identifiers and epochs survive a handover | provisional |

## 5. The membership grant (magic `bom` `0x01`)

| Item | Status |
| --- | --- |
| Keys: 0 `magic`, 1 `hall`, 2 `org`, 3 `subject`, 4 `serial`, 5 `revocation`, 6 `fields`, 7 `signature`, 8 `keyring` required, 9 `reveal` optional | signed |
| The certificate: type as section 1, certifier `org`, the BRC-52 binary form rebuilt with each field ciphertext in standard padded base64, signed under `[2, "certificate signature"]` with key id `"<type> <serial>"` | signed |
| Field names exactly `expiry`, `role`, `scope`; ciphertext lengths 68, 49 to 112, 49 to 560; keyring entries 80 bytes; reveal entries 32 bytes | signed |
| `expiry` exactly `YYYY-MM-DDTHH:MM:SSZ`; `role` 1 to 64 bytes of UTF-8; `scope` BRC-169 section 9.2's grammar without `*`, at most 512 bytes | signed |
| Scope verbs `member` and `admin`, neither qualified; an undefined verb authorizes nothing; a defined verb with any qualifier is denied | signed |
| **The revocation outpoint is the grant carrier's funding outpoint**, checked at admission (`grant-revocation`) | signed |
| A grant is current when its commitment is in the latest roll, its outpoint is positively known unspent (a node or index, or two sources, within 60 seconds for anything that moves value or releases a key), its scope grants `member`, its expiry has not passed (300 seconds of skew), and, unless the organization is opaque, it carries a reveal; at most 8 grants per subject in a roll | signed |
| The organization wraps only to keys with a current grant; rotation debt is a member with no current grant now; every transition in a group with debt first removes those keys | signed |
| Content rule order and labels: `grant-revocation`, `grant-signature`, `grant-reveal` | signed |

## 5A. The policy record (magic `bop` `0x01`)

Every row **provisional**, awaiting signature.

| Item |
| --- |
| Keys: 0 `magic`, 1 `hall`, 2 `org`, 3 `kind`, 4 `role`, 5 `permissions`, 6 `to`, 7 `expiry`, 8 `created`; all required, keys 9 and above unknown; the record at most 8192 bytes |
| `kind` 0 a role definition, 1 a binding; the cross-field rule `binding` (a definition's `to` and `expiry` are `null`; a binding's `to` is not) |
| The role-name grammar: 1 to 64 of lower-case ASCII letters, digits, `-`, `_` and `.`, compared byte for byte; a grant's `role` field outside it names no role |
| The permission grammar `service` / `action` / `resource`, ASCII and lower case, at most 256 bytes, `service` at most 16 and never `*`, `action` at most 32 and never `*`, `resource` either `*` or at most 192 characters with an optional trailing `*` |
| The matching rule: exact, or a `resource` prefix ending `*`; `service` and `action` always exact; no suffix wildcard, none inside a resource, no hierarchy; an asked permission carrying `*` is refused, never matched |
| `permissions`: 0 to 24, ascending, unique; in a definition the role's set, in a binding the attenuation, empty meaning the whole role; attenuation is the intersection with the definition |
| A binding's `to`: 16 bytes a group identifier, 33 bytes an identity key meeting the key rule and neither the organization nor the anyone key |
| `expiry` exactly `YYYY-MM-DDTHH:MM:SSZ` or `null`, read against the reader's clock with at most 300 seconds of skew |
| Effective permissions: the union of the grant's role (unattenuated) and every current binding to the subject's key or to a group whose latest token holds it (attenuated), and the empty set unless the subject is a current member. Nothing subtracts; there is no deny rule and no precedence |
| Current: the commitment is in the policy set of the latest token, the carrier is held, and a binding's `expiry` has not passed |
| More than one definition of one role in the set is read as the intersection of their permissions |
| A policy record is in the clear; the opaque setting does not cover it |
| Hosts check grammar, bounds and the carrier rules only: no host checks that a role is defined, that a group exists, that a subject is a member, or anything else about meaning, and no host enforces policy |
| The content rule label `binding`, and the key labels `role` and `permission` |

## 6. The epoch key and the wrap (magic `bow` `0x01`)

| Item | Status |
| --- | --- |
| BRC-369 section 2 verbatim: `k` uniform in `[1, n-1]`, fresh per epoch; symmetric key `SHA-256("metanet keyed content symmetric v1" \|\| k)`; commitment `SHA-256("metanet keyed content commitment v1" \|\| k)`, digest form only | signed |
| Release derivation: `[2, "keyed content release"]`, key id base64 of `SHA-256(epoch id \|\| epoch as 4 bytes big-endian)`, counterparty the member when wrapping and the organization when unwrapping | signed |
| Wrap keys: 0 `magic`, 1 `hall`, 2 `org`, 3 `group`, 4 `epoch`, 5 `epochId`, 6 `entries`; all required; 1 to 16 entries ascending by recipient | signed |
| An entry: exactly 182 bytes, BRC-78 serialization with version `42 42 10 33`, sender `org`, recipient neither `org` nor the anyone key, the derived key id, then a BRC-2 ciphertext of the 32-byte key | signed |
| Only the organization releases | signed |
| The member's check order: `token`, `entry`, `undecryptable`, `commitment` | signed |
| Writers key new content under the latest epoch, with positive evidence obtained within 60 seconds (at most 600): the token verifies and its output is unspent per a spend index; an unmined spender that is the next token keeping the epoch lets them go on; without the evidence they do not write | signed |
| Readers authenticate an object's author as a member of the epoch it names, and flag objects keyed under a superseded epoch | signed |

## 7. Names and questions

| Item | Status |
| --- | --- |
| Hall identifier `<name>_<suffix>`, 12 to 42 bytes: `<name>` 1 to 31 bytes of `[a-z]` and single underscores, starting and ending with a letter; `<suffix>` exactly 10 letters drawn uniformly at random once at the hall's creation | signed |
| Class names and members: free `policy`, `policy-after`, `role`, `role-after`, `bound`, `bound-after` (provisional), and `org`, `org-seq`, `roll`, `roll-after`, `member`, `groups`, `groups-after`, `member-of`, `member-of-after`, `group`, `group-seq`, `wraps`, `wraps-after`, `wrap`, `sweep`; priceable `history` `{hall, history, chain}`, `history-after` (spec section 10.2); no priced class is a superset of a free class | signed |
| `member` answers carry each grant's inclusion path in the roll as BRC-24 `context`: CBOR `[seq, index, size, [[bytes(32), bool], ...]]` | signed |
| `role` and `bound` answers carry each policy record's inclusion path in the **policy** root, in the same context shape | provisional |
| Query values validated as parsed; every member a string; the value rules of spec section 10.2; an unknown or missing member refused; an unknown hall refused | signed |
| Answer order: carriers by txid, groups by group id, tokens by `seq` then txid; `-after` strictly after the cursor | signed |
| Terms document at `<base>/ls_borg/terms`: `service`, `terms` (`1`), `classes` of `{class, satoshis}`; informative; the 402 is authoritative | signed |
| Retention floors: tokens and outpoint rows for the host's life; grants while in the roll and 31 days after; policy records while in the policy set and 31 days after (provisional); wraps of the current epoch, and of an ended epoch 31 days after the ending token was first seen | signed (floors: a later version may lengthen them, never shorten) |

## 8. Bounds that are a floor

Readers accept every record within these, and a later version may raise them
but never lower them: organization and group records 8192 bytes, grant and
wrap records 4096 bytes, 64 changes per list per transition, 65536 grants in a
roll, 1024 members in a group, 16 entries per wrap record, 64 keys per record,
a host's BEEF bound of at least 262144 bytes, a host's sweep input bound of
at least 1024. Signed as floors.

Provisional floors for the policy record: the record 8192 bytes, 24
permissions per record, a permission 256 bytes, a role name 64 bytes, 65536
policy records in a set, 64 `define` and 64 `retire` per transition.

## 9. Not frozen

- Funding tree sizes and output values; fee sources of tokens.
- Host policy: retention beyond the floors, lookup rate limits, prices, metric
  names.
- The answer limits (8 per question, 64 per page) and the superseded
  carriers kept per outpoint (8 wraps, 64 grants), which a later version may
  raise.
- `rotateEvery`'s schedule, which no host enforces.
- Anything the spec marks as a host's, a publisher's or a reader's choice.
- The limits of [limits.md](limits.md).
