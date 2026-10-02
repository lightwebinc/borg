# Limits

What an organisation publishes, how fast, and what a host answers are bounded
twice. The record bounds every reader accepts ([spec.md](spec.md) section 14)
are protocol and sit above everything here. The limits below are policy:
recommended defaults and caps for the organisation's tooling, members'
clients and hosts, none of them a wire format or a frozen bound
([frozen.md](frozen.md)), and a later version may change any of them without
a new record version.

A **hard cap** is refused by the client before anything is sent: exit 2,
with a message naming the setting, the limit and why, for example

```text
borg: tree_count 1200 is over the limit of 1000: every carrier's BEEF carries its whole funding tree (docs/limits.md)
```

A **recommendation** passed is a `warning: ...` line on stderr, and the
command goes on. A **host** limit is the host operator's, set in the host's
environment ([host.md](host.md)); a client cannot change it. The `borg`
command applies every row below that concerns the organisation's tooling
or a member's client ([usage.md](usage.md)).

## The table

### The organisation and its groups

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Members of a group | as needed | 1024 (the record bound) | Every removal wraps the new key to every remaining member | Warns above 256: a removal then costs at least 32 wrap carriers |
| Wraps per wrap record (`entries`) | 8 on the plane, 16 in unicast | 16 (the record bound) | An 8-entry carrier is 4 packets, 16 is 6 | Plane warns above 8 |
| Groups of an organisation | as needed | none in the records; a client lists 4096 a host (64 pages) | Each group is its own chain and costs one chain-tree output and one mined token to begin; the groups do not slow each other | Same |
| Grants in the roll | as needed | 65536 (the record bound) | The roll is one Merkle root; a member is checked by an inclusion path, the whole roll by reading it | A client reads at most 4096 grants a host (64 pages); a larger roll is checked one member at a time |
| Grants one key holds in the roll | 1 | 8 | A renewal overlaps the grant it replaces; more is a mistake | Same |
| Changes per transition (`admit`, `revoke`, `add`, `remove`) | as needed | 64 each (the record bound) | A larger change is several tokens, one block apart | Plane warns above 16 in all (a token and its parent then pass 4 packets); unicast: 64 |
| Transitions per chain | one at a time | one a block | Each token spends the one before it, mined; the next is built only once the previous is proven in the tooling's own headers. Different chains (the organisation's, each group's) move in the same block | Same |
| Propagation delay before publishing a mined token or sweep (`delay`) | 60 s after its block is in the publisher's headers | 0 s to 10 min | A host whose headers lack the block refuses it, and the plane does not deliver it again. So a change is at the hosts about one block and 60 s after it is decided | Plane and unicast; the publisher then confirms by lookup and resubmits |
| Blocks before publishing wraps | the token's own block | one further block when `signers` is above one | A transition displaced by a reorganisation in favour of another instance's must not have released its key | Same |
| Grant lifetime (`-expiry`) | 365 days | the year 9999 | An expired grant is rotation debt: the member is owed a removal | Same |
| Signer key's grant lifetime | 10 years, renewed by the tooling before any group transition | fixed | It is the organisation's own key, not a person's: nobody is there to renew it | Same |
| Window `org due` and `doctor` look ahead (`-within`) | 30 days | the flag | Long enough to renew a grant before its holder is removed | Same |
| Funding tree outputs (`tree_count`) | 32 | 1000 | Every carrier's BEEF carries its whole tree, about 49 bytes an output | Plane warns above 100 |
| Chain tree outputs (`chain_tree_count`) | 8 | 32 | A chain's first token carries its chain tree whole | Same |
| Trees kept apart | grant, record and chain trees, each under its own key and basket | fixed | A mined spend of a grant's funding output revokes it, so a tree that funds grants funds nothing else | Same |
| Tree minted ahead | one, when 4 outputs are left (half the tree when it has 4 or fewer) | one at a time per kind | The next tree is settled while the last outputs are spent, so a change seldom waits a block for a tree | Same |
| `object_bound` | 1048576 | 8388608 | BRC-149: 1 MiB on the open path, 8 MiB on an authenticated one; no plane admits more | Plane only |

### More than one home of an organisation

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Instances that can sign (`signers`) | 1 | 16 | Above one: wraps wait a block, the signer key is kept a member of every group, and each command first catches up when the node shows a latest token spent | Same |
| Concurrent changes to one chain | one wins | one mined a block | Two transitions on one tip spend the same output; the loser is discarded with its key, having released nothing, catches up and builds again (exit 1, then run it again) | Same |
| What a catch-up reads (`org sync`) | every token of every chain from seq 0, the whole roll, the wraps of every epoch, at every host | the reading caps below | It trusts no host: it folds each chain and reproduces the roll's root itself. Cost grows with the organisation's history times the host count | Same |
| What a catch-up asks the node | one spent-output question per coin in the pool, per latest token and per grant in the roll | none | The pool is made the home's unspent coins again, and a grant swept elsewhere is marked | Not on the plane |
| Funding trees two homes share | none | none; a tree another home spends is retired by `org sync` | A funding output answers one carrier: of two on one output a host answers one and keeps the other as superseded, and one sweep retracts both | Same |

### Publishing and reading

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Hosts named (`hosts`) | none | 16 | A reader asks every host it names; a unicast publisher uploads every object to each | Unicast warns above 5: past that the plane is what removes the cost |
| Quorum (`quorum`) | `all` | the host count | A quorum no host set can meet would refuse every publish | Unicast only, for carriers; warns below a majority. A token or a sweep always needs every host named, on the plane too |
| Tries per submission | 4, waiting 1, 2 and 4 s | fixed | A host that fails four tries is reported missed; the object stays persisted and the next command resends it, tokens in order | Plane: the facade, and each host named for mined objects; unicast: each host |
| Wait for the plane to deliver a wrap | 3.5 s a host (0.5, 1 and 2 s between lookups) | fixed | The plane delivers in well under a second; a host still without the wrap is offered it directly | Plane only |
| Request timeout (`timeout`) | 15 s | 1 s to 5 min | Bounds every request, so one host holds an object up for at most four timeouts and 7 s | Same |
| Answers | 8 a question, 64 a page | spec | Bounds an answer | Same |
| Pages a client reads per question | until an answer is short | 64 pages (4096 outputs) | Each page is a lookup and up to 64 verifications | Same |
| Object a reader takes | as answered | 1 MiB | No borg object is near it | Same |
| Writer freshness (`freshness`) | 60 s | 600 s | A writer keys content under the latest epoch it verified this recently (spec section 6.5). A transition that begins an epoch pauses writers for about a block | Same |
| Revocation check age, for anything that moves value | 60 s | the reader's resolution lifetime | BRC-169 section 4.2 | Same |
| Expiry skew | 300 s | 300 s (spec) | BRC-169 section 9.6 | Same |
| What `fill` copies | every token, roll grant and wrap a host lacks and another answered | the reading caps | Each copy is one submit and one lookup at the host that lacks it | Off the plane, or a host a publisher missed |

### Revoking and the kill switch

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Sweeps in flight per home | one | one | A sweep is persisted, settled and waited for before the next is built, so no output is swept twice | Same |
| Sweeps per revocation | one per grant tree the grants sit on | one a block | A sweep spends outputs of one tree | Same |
| Sweeps per `kill` | one per grant tree and per record tree the home holds | one a block | Every output of the tree in one transaction; a tree at the cap of 1000 outputs is within every host's floor of 1024 sweep inputs | Same |
| Wait for a sweep, a token or a tree to mine | 10 min | fixed | Past the wait it stays persisted and the next command waits again | Same |
| Wait for a fee coin | up to the same 10 min, only when every coin is change not yet mined | fixed | A tree minted ahead can take the last proven coin; its change becomes spendable when it mines | Same |

### Paying and settling

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Payments one coin can make to one host | one | none | Two unbroadcast payments spending one coin both verify and only one can mine: the host refuses the second (409). A payer's next payment spends the change of its last, which is another coin | Not on the plane |
| `borg history` refused for its session's budget (429) | waits as the host says, 1 to 30 s, and asks once more | one retry | A second refusal ends the command, exit 3; nothing was paid for a refused request | Not on the plane |
| Price the client pays for one priced question (`-max-sats`) | 1000 sat | the flag | A price is per question (one page); a host that asks more is asked by the user, never paid by default | Not on the plane: the terms route is HTTP |
| A payer's unmined payments in a row | each spends the change of the last | 16 | A payment is mined only when someone broadcasts it. The change of the payer's own unmined payment is spendable by its next payment, which carries that payment in its BEEF; past 16 the payer waits for a block. For anything but a payment the change waits until it mines | Not on the plane |
| Coins a payer needs | one | none | See the row above. A payer that asks more than 16 priced questions a block holds several coins (`fund` more than once, or several payments to the fund address) | Not on the plane |
| Payments one `payee settle` has broadcast and not yet mined (`-in-flight`) | 16 | 1 to 64 | All are broadcast before any is waited for, so a run takes about one block; a payment that spends an unmined payment has that one broadcast first | Not on the plane |
| Wait for a payment to mine | 10 min | fixed | Past it the payment is `NOT SETTLED` and tried again next run. One whose input is spent elsewhere is refused at once, recorded, and never tried again | Not on the plane |

### Hosts

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| BEEF bound (`BORG_MAX_BEEF`) | 262144 bytes | at least 262144 (a floor, never lowered) | A token of 64 additions and 64 removals with its parent, or a carrier from a 1000-output tree, fits | Same |
| Sweep inputs (`BORG_MAX_SWEEP_INPUTS`) | 1024 | at least 1024 | A sweep of a whole 1000-output tree is admitted | Same |
| Tokens kept per seq of a chain | 8: those not `pending` first, then the lowest txids | host policy | Bounds what a fork can make a host keep. A `pending` token turned away is read back from storage once its predecessor is decided | Same |
| Superseded carriers kept per funding output | 8 wraps, 64 grants | host policy | Evidence that an output was used twice, bounded | Same |
| Retention of what a host no longer answers (`BORG_RETENTION_DAYS`) | 31 days | at least 31 (spec section 11.3) | Longer retention of ended epochs' wraps is what a host sells | Same |
| Spends of outpoints no carrier used (`BORG_SPEND_TABLE`) | 65536, oldest dropped first | at least 65536 | A sweep can arrive before the carrier it retracts | Same |
| Outpoint rows (`borg-rows.jsonl`) | kept for the host's life | none | It is what keeps a retracted carrier retracted across restarts; back it up ([host.md](host.md)) | Same |
| Carrier lines held in memory | every carrier ever admitted, about 1 KB of memory and 300 bytes of journal each | none: not bounded. 1,000,000 carriers are about 1 GB of memory | The winner of a funding output is the lowest commitment ever admitted on it, so every one is kept. `borg_carrier_lines` counts them; size Node's heap for it | Same |
| BRC-104 sessions the terms route keeps (`BORG_SESSIONS`, `BORG_SESSION_TTL`) | 10000, each forgotten after 600 s idle | host policy | A handshake is unauthenticated: the store must not grow without end. Past the cap the least recently used goes and its client shakes hands again | Not on the plane |
| BRC-104 handshakes the terms route answers (`BORG_HANDSHAKES_PER_SEC`, `BORG_HANDSHAKE_BURST`, `BORG_HANDSHAKES_PER_ADDR_PER_SEC`, `BORG_HANDSHAKE_ADDR_BURST`) | 4 a second, 8 at once; per remote address (IPv6 per /64) 1 a second, 4 at once | host policy; over it answered 429 with `Retry-After`, before any signature | The session cap bounds memory, not work: each handshake is a signature on the host's own process. An honest client shakes hands once a session | Not on the plane |
| Signed responses one BRC-104 session is given (`BORG_RESPONSES_PER_SEC`, `BORG_RESPONSE_BURST`) | 5 a second, 20 at once | host policy; over it answered 429 with `Retry-After` and no signature | Every authenticated answer is a signature on the host's own process: one handshake must not buy them without end. A reader walking pages asks far fewer | Not on the plane |
| Authenticated requests remembered, to refuse one sent twice | the last 65536 | fixed | A replay is refused 401 before any signature; the session budget bounds an older one | Not on the plane |
| Lookup rate a host accepts | host policy | host policy; 429 with `Retry-After` | Lookup is an unauthenticated surface | Same |

## Where the numbers come from

**Sizes, measured.** `internal/limits` builds real objects with bcommon: grant
and wrap carriers minted from a mined funding tree (Atomic BEEF), and
chain-token transactions (the token, a predecessor input, a fee input and
change) published with a predecessor of the same size (BEEF V1, both
proven), each proof a Merkle path of 16 levels (a block of about 65,000
transactions), and measures the BEEF a publisher sends. A packet carries 1128 bytes of
object (the IPv6 minimum MTU of 1280 bytes less the IPv6, UDP and object
fragment headers).

| Object | Record | BEEF | Packets |
| --- | --- | --- | --- |
| grant carrier, tree of 32 | 849 | 3,497 | 4 |
| grant carrier, tree of 100 | 849 | 6,830 | 7 |
| wrap carrier, 1 entry, tree of 32 | 309 | 2,959 | 3 |
| wrap carrier, 8 entries, tree of 32 | 1,597 | 4,247 | 4 |
| wrap carrier, 16 entries, tree of 32 | 3,069 | 5,720 | 6 |
| group token, no change (a rotation) | 242 | 2,522 | 3 |
| group token, 2 added | 312 | 2,665 | 3 |
| group token, 1 removed | 277 | 2,594 | 3 |
| group token, 16 added | 802 | 3,646 | 4 |
| group token, 64 added and 64 removed | 4,724 | 11,487 | 11 |
| organisation token, 3 admitted | 278 | 2,598 | 3 |
| group token at seq 0, chain tree of 8 | 296 | 2,373 | 3 |
| group token at seq 0, chain tree of 32 | 296 | 3,550 | 4 |

Each entry of a wrap is 182 bytes plus 2 of CBOR framing; each key in a
change list 35; each commitment 34. A chain token carries its predecessor's
transaction and both proofs, so it is about 2.5 KB and 3 packets for an
ordinary change, and grows with the change lists of both.

**Why small on the plane.** An object larger than one packet is carried as
fragments. Every lost fragment is repaired, but the whole object must be
reassembled within a fixed window, and a fragment that needs a second or third
repair round at a long round trip can land after the object was given up. In
measurements at 1 % packet loss no object of 1 or 8 packets was lost and
about 1 % of 16-packet objects were. Every borg object at its default (a
tree of 32, 8 entries a wrap, up to 16 changes a token) is at most 4
packets. At the record bounds a token of 64 additions and 64 removals is 11,
and a carrier from a tree of 1000 outputs is about 46 packets, which is why
the plane warns well below both.

**Unicast.** No fragments: a host takes each object in one HTTP request, so a
unicast publisher can use the full bounds. What grows instead is its upload
and its retries, multiplied by the host count.

**Cost of a removal.** One mined token (about 2,600 bytes as published) and, for a group
of `n`, `ceil((n - 1) / 8)` wrap carriers on the plane, never mined. A group
of 100 is one token and 13 carriers, about 58 KB delivered to every host in
the hall. At a fee of 1 satoshi per byte the token costs a few hundred
satoshis, and a funding output pays for each carrier.

**Time of a removal.** A token is published only once mined, and the next
token of the same chain is built on it, so each chain moves at most once a
block. A removal takes effect for readers about one block after it is
decided, plus their freshness bound.

**Rate of change.** Each chain moves at most once a block, and a token is
published 60 s after its block reaches the publisher's headers. Adding ten
people to one group is one token (up to 64 keys a token); removing one
person from ten groups is ten tokens, one in each group's chain, which can
all mine in the same block. An offboarding is an organisation token (the
roll), one sweep per grant tree, and one token per group: on a chain with
a block every ten minutes, about three blocks.

**Catching up.** `org sync` reads the organisation as a stranger would. For
an organisation of `g` groups with `t` tokens and `e` epochs each, at `h`
hosts, that is about `h * (g * (t + e) + roll pages)` lookups, each answer
verified against the headers. It runs only when another home changed
something: with `signers` above one, a command asks the node whether any of
the home's latest tokens is spent (one question a chain) and each host for
the list of groups (one a host), and reads the rest only when the answer is
yes.

**The terms route.** A BRC-104 handshake costs the host a signature, on the
host's own process. On a test host 40 priced questions from fresh
identities at one address, 8 at a time, were answered in about 3 s: 5 or 6
with the 402 and the rest refused 429 before any signature, and a client
that paid a moment later was answered. The session cap holds memory at
10000 sessions whatever the flood; the handshake budget is what bounds the
work.

**Paying in a row.** A payment for a priced question is the payer's own
transaction, handed to the host and mined when it is broadcast: by the
payer's settlement leg when it has one, and by the payee when it settles.
Its change is the payer's, so the payer spends it in its next payment at
once and the host verifies that payment through the one before. Sixteen in
a row keeps a payment's BEEF to a few kilobytes. A payer never spends one
coin twice: the pool hands each coin out once, and a payment the host did
not accept gives its coin back.
