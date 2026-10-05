# borg user guide

This guide is for the people who run an organisation with borg and for
its members: what borg does, how to set it up, and how to use it day to
day. [QUICKSTART.md](../QUICKSTART.md) runs all of it on one machine in a
few minutes; [usage.md](usage.md) is the reference for every command and
setting; [spec.md](spec.md) is the contract the records follow.

## 1. What this is

borg gives an organisation a membership roll and its groups a shared key
that changes whenever anyone leaves.

- **Membership is a certificate.** The organisation issues each member a
  BRC-52 certificate with a role, a scope and an expiry. Any BRC-52
  verifier can check it, and one mined spend revokes it everywhere.
- **Changes are ordered by the chain.** The roll and each group are chains
  of mined tokens. Two changes made at once spend the same output, and only
  one is ever mined: no server decides which.
- **Removal is a rekey.** Every overlay host refuses a removal from a group
  that does not begin a new epoch, and each epoch's key is sampled fresh
  and wrapped only to the members who remain. A removed member keeps what
  they could read and reads nothing after.
- **Hosts are replicas.** A host can withhold an answer; it cannot forge
  one. Every member checks every answer against block headers, and roots
  over the roll and each group's members make a withheld grant visible.

What borg does not hide: who belongs to which group and when they joined
or left are public to everyone who reads the hall (spec section 16).

## 2. Install

Build `bin/borg` with `make borg` (Go 1.27.1 or later), or run the image
`ghcr.io/lightwebinc/borg` with a volume at `/home/nonroot/.borg`. A host is
the `borg-host` image or the module on a reference overlay host
([host.md](host.md)). `borg doctor` shows the home and asks every
configured endpoint whether it answers; it changes nothing.

## 3. Concepts

| Word | Meaning |
| --- | --- |
| home | a directory holding one identity: its key, its coin, its state and every epoch key it holds. Secret |
| organisation | an identity key; the home that runs `org create` is the organisation |
| hall | the topic `tm_borg_<name>_<suffix>` an organisation lives in, shared by any number of organisations |
| grant | a member's BRC-52 certificate; its revocation outpoint is the output that funds it |
| roll | the grants the organisation's latest token commits to, by a Merkle root |
| group | a chain of tokens naming its members; each has epochs |
| epoch | one generation of a group's key; every removal begins one |
| wrap | an epoch key encrypted to one member, in a record of up to 16 |
| rotation debt | a group member whose grant is no longer current; the group's next change removes them |
| signer key | a key every home of the organisation derives from its identity key; as a member of a group it is wrapped each epoch's key, which is how a second home comes to hold it |
| token, carrier | a token is a mined transaction (one per change of a chain); a carrier is an unmined one that holds a grant or a wrap and spends a funding output |
| sweep | a mined spend of funding outputs: it revokes the grants and retracts the carriers they funded |

## 4. Configuration

Settings come from flags, `BORG_<KEY>` variables, and a `key = value`
file at `~/.borg/config` ([usage.md](usage.md#configuration)). An
organisation needs a hall, the hosts, a header source, a node (`asset`), a
settlement leg (`settle`), and `facade` on the plane. A member needs the
hall, the organisation's key (`org`), the hosts, a header source, and the
node for revocation checks and writer evidence.

```text
# ~/.borg/config, a member
hall       = example_qzxkvbmwtr
org        = 02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5
hosts      = https://host-a.example.com,https://host-b.example.com
header_url = https://headers.example.com
asset      = http://192.0.2.10:8090
```

No address has a default: a default would send an organisation's records,
or a member's questions, to a server nobody chose.

## 5. Running an organisation

```console
$ borg init                     # once: the organisation's key
$ borg fund -txid <txid>        # import a payment to the fund address
$ borg hall new example         # or use a hall your hosts carry
$ borg org create 'Example' -member <alice> -member <bob>
$ borg group create engineering -member <alice> -member <bob>
```

The home's identity key is the organisation: it signs every grant, every
token and every wrap. Keep the home at mode 0700 and back it up as a
secret; `borg org show` prints what it holds, and `borg doctor` what it
still owes.

Each change waits for its token to be mined, waits the propagation delay
(60 s), publishes it, and makes the wraps it needs. A change therefore
takes about a block and a minute, and each chain (the organisation's, each
group's) moves once a block ([limits.md](limits.md)). If a command stops
part way (a host down, the network slow, the machine switched off), what
it built is kept in the home and the next command finishes it first.

The same holds for a funding tree. If a command stops while it is minting
one, the next command that publishes asks the node what became of it and
tells you: `funding tree ... is recovered` when the tree reached the chain
(nothing is lost), or `... never reached the chain: its fee coin ... is
unspent and back in the pool` when it did not. You have nothing to do; if
the node cannot be asked, the command says so and the next one asks again.

What it costs: each change is one mined transaction of about 2.5 KB, a
few hundred satoshis at 1 satoshi a byte. Each grant and each batch of
wraps rides a funding output of one satoshi and is never mined; the tree
that holds 32 such outputs is one more mined transaction.

## 6. Members

```console
$ borg grant issue <carol> -role engineer -expiry 8760h
$ borg org add <carol> -group engineering     # grants her first if she holds no current grant
```

A grant names a role (a label), a scope (`member` is what makes a
membership) and an expiry (a year unless `-expiry` says). `-opaque` at
`org create` publishes grants without their fields in the clear: readers
then see that a key is a member, not its role.

A member sets up a home and reads:

```console
$ borg init
$ borg member <your key>         # your grants, opened through your keyring
$ borg open engineering          # every epoch key you can open
$ borg member-of <your key>      # your groups
```

`open` keeps every key it opens in the member's home, so later reads need
no host, and says which epochs it cannot open and why. Before keying new
content under a group's epoch, run `borg open <group> -write`: it checks,
through the member's node, that the group's latest token is still the
latest, and refuses (exit 3) when it cannot be sure. An application keys
its own content under the epoch key; borg delivers the key.

**Expiry.** A grant that expires makes its holder rotation debt in every
group. `borg org due` lists the removals owed and the grants of group
members that expire within 30 days; renew one with `borg grant issue <key>`
before it lapses. `borg org due -apply` makes the removals that are due
now, each with a new epoch; run it from a timer and each removal follows
its expiry by at most the timer's interval. Nothing is removed early.

## 7. Groups and epochs

A group is a chain of tokens. Each token names who was added and removed
and carries a root over the members; an epoch is the stretch of that chain
under one key. The key of an epoch is sampled at random by the
organisation, committed to in the token that begins the epoch before
anyone holds it, and wrapped to each member.

```console
$ borg group create engineering -member <alice> -member <bob>
$ borg group list
$ borg group show engineering     # as any reader: the chain folded, the members from the wraps
```

- **Adding** someone keeps the epoch: the joiner is wrapped the current
  key and reads what the epoch already holds, as joining a room means.
  `-rotate-on-join` at `group create` makes every addition begin a new
  epoch instead.
- **A key that was removed and comes back** always gets a new epoch, so it
  never reads what was written while it was away.
- **Rotating** without a change of members is `borg epoch rotate <group>`:
  on a schedule (`-rotate-every` records the one you intend, and `doctor`
  says when it is overdue) or after a suspected leak of a key.

## 8. Removal and rotation

```console
$ borg org remove <bob> -group engineering      # a new epoch
$ borg org rekey <alice> <alice's new key>      # her key changed: a new epoch in every group
$ borg org remove <bob>                         # offboard: every grant revoked, every group left
```

A removal begins a new epoch: hosts refuse a token that removes anyone
without one. The new key is wrapped to the members who remain, and to
nobody else. Every transition of a group also first removes its rotation
debt, so a member whose grant was revoked or has expired leaves with the
group's next change, whatever that change is.

Removal takes effect when the removing token is mined and seen. Until then
the member is still a member: the window is one block, the propagation
delay, and each writer's freshness bound (60 s by default).

### What a removed member can still read

They **can** still read:

- everything keyed under an epoch whose key they hold: the epoch they were
  removed from and every earlier one they were in. Rotation is not
  retroactive;
- anything a writer keys under the old epoch before it sees the new one. A
  writer that checks (`open -write`) does so for at most its freshness
  bound after the removal mines; a writer that does not check keeps doing
  it;
- anything a current member shows them. Every member of an epoch holds the
  same key, so a leak names nobody;
- every public fact: the tokens, the grants and their revealed fields, who
  the wraps are addressed to, and when each change happened.

They **cannot** read anything keyed under the new epoch or a later one: the
new key is independent of the old, and no wrap of it names them. They come
back only by a mined transition everyone sees, which begins a new epoch.

If what was written before must stop being readable by them, rotation does
not do it: nothing can. Rotate promptly, and treat what the removed member
could read as read.

## 9. Revocation

```console
$ borg grant revoke <key>              # by the roll and by sweep
$ borg grant revoke <key> -by sweep    # or one of the two
```

A grant is revoked two ways. The next organisation token leaves it out of
the roll, which every reader of borg's records sees. And a mined sweep
spends its revocation outpoint, which every BRC-52 verifier sees and which
makes every host retract the grant. A revoked member still in a group is
rotation debt until the group's next change removes them.

Revocation is as fresh as the node you ask: a reader with no node cannot
tell a revoked member from a current one, and borg then says so rather
than guess. A sweep is not erasure: a copy of a grant anyone kept is still
a copy, only no longer a valid certificate.

**The kill switch.** `borg kill -confirm KILL` lists, and with `-yes`
makes, one sweep per grant tree and per record tree the home holds. It
revokes the whole roll and retracts every grant and wrap at every honest
host, one block a tree, and cannot be undone. The chains stay (they are
mined), every member is then without a current grant, and epoch keys
members already opened stay opened. Use it when the organisation's key or
home is lost to someone else, or to end the organisation. It does not wait
for work an earlier command left unfinished, and an output another home
already swept is passed over, its sweep taken as done. If it says outputs
are spent by a transaction not yet mined, run it again after a block.

## 10. A host that missed something: `fill`

A host that was down off the plane, or was left out of a unicast publish,
lacks what the others hold, and nothing resends it. `roll`, `group show`
and `open` say so (`DISAGREES`, exit 3). Anyone who reads the hall repairs
it, with no coin and no key:

```console
$ borg fill                 # the organisation's chain, the roll, every group
$ borg fill engineering     # or the groups named
filled  group 5a0c… seq 5 3f9ac4107d21 at https://host-b.example.com
filled  wrap 9ade7f2e0b15 at https://host-b.example.com
filled  2 object(s) at 1 host(s)
```

`fill` replays to each host what another host answered and it lacks: each
missing token with its parent, each grant of the roll, each wrap of every
epoch a host still answers. Everything is verified against your headers
before it is sent and confirmed by a lookup afterwards. Run it again:
`filled  0 object(s)` means the hosts agree.

A chain's first token needs its chain tree, which no host keeps and a node
may have dropped once all its outputs were spent. The organisation's home
keeps it: run `fill` there and the tree comes from the home, checked like
anything else. From any other home, a host missing a chain's first token
can be filled only while the node still answers its tree; otherwise `fill`
names the tree and asks you to run it from the organisation's home.

## 11. A second home: `sync`

An organisation is one identity key, and more than one home can hold it: a
second device, a standby, or a new machine after the first was lost.
Everything such a home needs is at the hosts, except the epoch keys, which
reach it the way they reach any member: in a wrap, addressed to the
organisation's **signer key**.

Prepare, on the home that runs the organisation:

```console
$ borg org signer          # grant the signer key and add it to every group
```

With `signers = 2` (or more) in the configuration this happens by itself:
every group the home creates or changes includes the signer key, and wraps
wait one further block. The signer key shows as one more member of each
group (`org show` and `group show` mark it).

Then, on the other machine:

```console
$ borg init -from identity.json    # the first home's identity file, carried by hand
$ borg fund -txid <txid>           # its own coin
$ borg -hall <hall> org sync
sync    took 2 organisation token(s), 4 group token(s) (1 new group(s)), 3 grant(s), 2 epoch key(s)
missing group 5a0c… epoch 0: the signer key was not a member of this epoch
```

`org sync` reads the organisation as any reader does (every token against
your headers, the roll's root reproduced) and opens each epoch key from the
wrap to the signer key. After it the home runs the organisation.

With `signers` above one, two homes can both be in use. When both change
the same chain at once, one transition is mined and the other is discarded
(exit 1, `lost`), having released nothing; run the command again and it
catches up by itself and makes the change on the new tip. Each command
also catches up first whenever the node shows that another home moved a
chain.

**What `sync` cannot bring back:**

- the key of an epoch the signer key was not a member of (it is named as
  `missing`). Its members still hold it. If it is the current epoch, this
  home wraps it to nobody until it opens it, and an addition made from this
  home begins a new epoch, which it holds (so does `borg epoch rotate
  <group>`);
- a grant another home issued and no token admitted yet;
- another home's unpublished work, funding trees and coin. Each home funds
  itself and mints its own trees. The trees of every grant and wrap the
  hosts answer are recorded, so this home can still revoke them by sweep;
- the epoch key of a transition that lost a race. It was discarded, and
  nobody needs it: nothing was released under it.

Do not copy a whole home that is still in use: two copies hold the same
coins and the same funding trees. If you did, run `borg org sync
-retire-trees` on the copy before it changes anything.

## 12. Plane or unicast

| | Plane (`mode = plane`, `facade`) | Unicast (`mode = unicast`) |
| --- | --- | --- |
| A grant or a wrap | submitted once to the facade; every subscribed host receives it; counts once `quorum` hosts answer it by lookup | submitted to each host; counts once `quorum` hosts took it |
| A token or a sweep | to the facade and to every host named | to every host; needs all of them |
| A host that was down | repaired by the plane within its window; every object is also looked for at each host and offered directly | misses what was sent meanwhile; `fill` repairs it |
| Object size | kept small: at most 4 packets by default | one HTTP request an object, up to the record bounds |
| Cost that grows with hosts | none | every object uploaded once per host |
| Priced questions | not on the plane: the terms route is HTTP | the same |

Either way a reader asks every host it names and compares the answers.

## 13. Priced questions and the payee

The free questions answer everything a member needs. A host may price the
bulk walk of a chain: `borg history -chain <org|group id>` pays its 402
from the home's coin (at most `-max-sats`), one page, or every page with
`-all` (at most `-budget` in all, 16000 sat unless you say otherwise).
One question is paid for once: a host that asks for a second payment is
refused, and a payment that was sent is kept as made even when the host
does not answer, so its coin is never spent twice. One coin is enough for
several questions in a row: each payment spends the change of the one
before, up to 16 unmined at once ([limits.md](limits.md)). A host's operator settles
what it was paid with `borg payee settle <payments.jsonl>` from the payee's
home, on a schedule.

## 14. Limits

The ones you meet first; the full table, with reasons, is
[limits.md](limits.md).

| Setting | Default | Cap |
| --- | --- | --- |
| Members of a group | as needed | 1024; warns above 256 |
| Wraps per record | 8 on the plane, 16 in unicast | 16 |
| Grants in the roll | as needed | 65536 |
| Changes per token | as needed | 64 each; plane warns above 16 |
| Changes per chain | one a block, published 60 s after it mines | one a block |
| Hosts | none | 16; unicast warns above 5 |
| Quorum (unicast carriers) | all | the host count |
| Price paid for one question | at most 1000 sat | `-max-sats` |
| Payments in a row from one coin | each spends the last one's change | 16 unmined |

## 15. Troubleshooting

Run `borg doctor` first: it shows the home, the pool, the trees, the
organisation, anything persisted and not yet published, a sweep in flight
or failed, each group's rotation debt, and whether every endpoint answers.

| Exit | Meaning | What to do |
| --- | --- | --- |
| 0 | done; everything asked verified | nothing. A disagreement between hosts that did not affect the answer is still printed |
| 1 | refused: the evidence contradicts what was asked | read the message: it names the host, the token or the reason |
| 2 | usage, configuration, transport, a quorum not met, a price over `-max-sats` or `-budget`, or a transition a leg refused that may still mine | fix the named setting; what was persisted is sent or published by the next command |
| 3 | incomplete: it proves nothing either way | ask again, ask other hosts, or `fill` |

Exit 1, by message:

- `CANNOT OPEN ... removed at seq`: you are not a member of the current
  epoch. You keep the epochs you held.
- `lost: its predecessor is spent by`: another home of the organisation
  changed the same chain first. Nothing was released. Run the command
  again; with `signers` above one it catches up by itself, otherwise `borg
  org sync` first.
- `lost: its fee coin is spent by`: another home holds the same coin (a
  copied home). `borg org sync`, then run it again.
- `not current, revoked`: the grant's revocation outpoint is spent.
- `catching up: ... a fork`: this home and the hosts hold different mined
  tokens at one seq. Do not go on from this home; read the chain with
  `group show` and `roll` and decide which home is right.

Exit 2, by message:

- `fee input: ... no spendable output`: fund the home (`borg fund`). If it
  says coins are change not yet mined, wait for a block. If it says a
  funding tree minted ahead holds a coin and has not settled, run the
  command again: the change returns once the tree settles.
- `1 of 2 host(s) took it and the quorum is 2`, or on the plane `1 of 2
  host(s) named answer it and 2 must`: a host did not take an object, or
  does not answer it. It is kept, and the next command publishes it first.
- `refused (...), and its predecessor is not spent by another transaction:
  it is kept`: a settlement leg refused a transition that nothing beat.
  It may be mining already. Run the command again; it is sent and waited
  for again, with the same key.
- `which the node could not show`: something spends the transition's
  predecessor and the node cannot show it yet. Nothing is discarded; run
  the command again.
- `the node could not say what spends its predecessor`: a leg refused the
  transition and the node does not say whether its predecessor is spent
  (a status other than unspent or spent, as for an output it pruned or
  could not read). Nothing is discarded; run the command again.
- `a name is one line of plain text`: the name holds a line break or a
  control character.
- `the transition breaks the host's rules` or `would be reported`: the
  tooling will not build what a host would refuse or an index would flag.
  The message names the rule.

Exit 3, by message:

- `REFUSED`: a host answered something that does not verify (bytes
  changed, or a wrap of another epoch). It is not used, the host is named,
  and what was read comes from the hosts whose answers verify. Ask other
  hosts, and look at the named one.
- `none verifies against the header source`: every token the hosts
  answered was refused. Check `header_url` (`borg doctor`) before blaming
  the hosts; after a reorganisation set `asset` so stale proofs are
  replaced.
- `DISAGREES`: a host lacks a token another answered: it is behind, or
  withholding. `borg fill`.
- `an older state`: the hosts answer an older chain than your home
  verified. Ask other hosts.
- `WITHHELD`: the hosts answer a later token of the chain and not one
  between: what was read may be an older state. Ask other hosts, or
  `borg fill` from a host that holds the whole chain.
- `the chain is not read to its end`: `history -all` reached its
  `-budget`. Give it a larger one, or read the rest with `-after`.
- `refused the paid request for its session's budget (429)`: the payment
  was sent and is kept; ask again later.
- `no host answers a wrap`: the organisation has not published your wrap
  yet (it follows the token by a block where several homes sign). Ask
  again shortly.
- `PENDING`: the same, seen by `group show`.
- `a writer does not write`: no positive evidence that the group's latest
  token is the latest. Check `asset`. `the node does not say whether the
  output is spent` means the node answered neither unspent nor spent for
  the token's output; ask again shortly.
- `removal(s) due`: `borg org due -apply`.
- `the key of the current epoch ... is not open here`: after `org sync`,
  the other home has not published its wraps yet, or the signer key is not
  a member of the group. Ask again after a block, run `borg org signer` on
  the home that holds the key, or rotate the group.
- `the hosts answer N of the M grant(s) in the roll`: no host holds the
  whole roll, so `org sync` took nothing. `borg fill`, then sync again.

In `doctor`:

- `ROTATION DEBT`: a member's grant is no longer current. `borg org due
  -apply`, or any change of the group, removes them.
- `KEY NOT HELD`: the group's epoch was begun by another home. `borg org
  sync`.
- `INCONSISTENT`: a token the index flags sits in the current epoch. `borg
  epoch rotate <group>` begins a clean one.
- `outbox`, `transition`, `sweep ... in flight`: work a previous command
  persisted. The next command finishes it.

## 16. Where to go next

- [usage.md](usage.md): every command, setting and exit code.
- [limits.md](limits.md): every default and cap, and why.
- [host.md](host.md): run a host, price a question, settle, back up.
- [spec.md](spec.md) and [frozen.md](frozen.md): the records, and what
  cannot change once published.
