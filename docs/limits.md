# Limits

What an organisation publishes, how fast, and what a host answers are bounded
twice. The record bounds every reader accepts ([spec.md](spec.md) section 14)
are protocol and sit above everything here. The limits below are policy:
recommended defaults and caps for the organisation's tooling, members'
clients and hosts, none of them a wire format or a frozen bound
([frozen.md](frozen.md)), and a later version may change any of them without
a new record version.

A **cap** is refused by a client before anything is sent, with a message
naming the setting and the limit. A **warning** is printed and the client
goes on.

## The table

| Limit | Default | Cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Members of a group | as needed | 1024 (the record bound) | Every removal wraps the new key to every remaining member | Warn above 256: a removal then costs at least 32 wrap carriers |
| Entries per wrap record | 8 | 16 (the record bound) | An 8-entry carrier is 4 packets, 16 is 6 | Plane: 8. Unicast: 16 |
| Changes per transition (`admit`, `revoke`, `add`, `remove`) | as needed | 64 each (the record bound) | One token per change keeps every change ordered; a larger change is several tokens, one block apart | Plane: warn above 16 in all (a token and its parent then pass 4 packets); unicast: 64 |
| Transitions per chain | one at a time | one in flight | Each token spends the one before it, mined; the next is built only once the previous is proven in the tooling's own headers | Same |
| Funding tree outputs | 32 | 1000 | Every carrier's BEEF carries its whole tree, about 49 bytes an output | Plane: warn above 100 |
| Chain tree outputs | 8 | 32 | A chain's first token carries its chain tree whole | Same |
| Trees kept apart | grant, record and chain trees, each under its own key and basket | none | A mined spend of a grant's funding output revokes it; sweeping every tree revokes the whole roll | Same |
| Propagation delay before publishing a mined object | 60 s after its block is in the publisher's headers | 10 min | A host whose headers lack the block refuses it and the plane does not deliver it again | Plane and unicast; the publisher then confirms by lookup and resubmits |
| Blocks before publishing wraps | the token's own block | one further block when more than one tooling instance can sign | A displaced transition must not have released its key | Same |
| Writer freshness | 60 s | 600 s | A writer keys content under the latest epoch it verified this recently (spec section 6.5). A transition that begins an epoch pauses writers for about a block | Same |
| Revocation check age, for anything that moves value | 60 s | the reader's resolution lifetime | BRC-169 section 4.2 | Same |
| Expiry skew | 300 s | 300 s (spec) | BRC-169 section 9.6 | Same |
| Hosts named | none | 16 | A reader asks every host it names; a unicast publisher uploads every object to each | Unicast warns above 5 |
| Quorum | all | the host count | A quorum no host set can meet would refuse every publish | Unicast only; tokens and sweeps always need every host named |
| Tries per submission | 4, waiting 1, 2 and 4 s | fixed | A host that fails four tries is reported missed; the object stays persisted and is resent, in order for tokens | Plane: the facade, and each host named for mined objects; unicast: each host |
| Request timeout | 15 s | 1 s to 5 min | Bounds every request | Same |
| Pages a client reads per question | until an answer is short | 64 pages (4096 outputs) | A larger roll is checked one member at a time by the `member` answer's inclusion path | Same |
| Answers | 8 per question, 64 per page | spec | Bounds an answer | Same |
| Host retention | the floors of spec section 11.3 | host policy above them | Longer retention of ended epochs' wraps is what a host sells | Same |
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
