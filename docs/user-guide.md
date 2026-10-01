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
([host.md](host.md)).

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

## 5. Running the organisation

```console
$ borg init                     # once: the organisation's key
$ borg fund -txid <txid>        # import a payment to the fund address
$ borg hall new example         # or use a hall your hosts carry
$ borg org create 'Example' -member <alice> -member <bob>
$ borg group create engineering -member <alice> -member <bob>
```

Add someone to a group (granting them first when they hold no current
grant), remove them, or change their key:

```console
$ borg org add <carol> -group engineering
$ borg org remove <bob> -group engineering      # a new epoch
$ borg org rekey <alice> <alice's new key>      # a new epoch in every group
```

Offboard someone entirely (every grant revoked by the roll and by sweep,
and a removal from every group):

```console
$ borg org remove <bob>
```

Each change waits for its token to be mined, then publishes it and the
wraps it needs. If a command stops part way (a host down, the network
slow), what it built is kept in the home and the next command finishes it
first: run `borg doctor` to see what is pending.

A key that was removed and comes back always gets a new epoch, so it never
reads what was written while it was away. `-rotate-on-join` on a group
makes every addition rotate. `borg epoch rotate <group>` rotates on a
schedule or after a suspected leak; `doctor` says when a group's epoch is
older than its `-rotate-every`.

## 6. Being a member

```console
$ borg init
$ borg member <your key>         # your grants, opened through your keyring
$ borg open engineering          # every epoch key you can open
$ borg member-of <your key>       # your groups
```

`open` keeps every key it opens in your home, so later reads need no host,
and says which epochs you cannot open and why. Before you key new content
under a group's epoch, run `borg open <group> -write`: it checks, through
your node, that the group's latest token is still the latest, and refuses
(exit 3) when it cannot be sure. An application keys its own content under
the epoch key; borg delivers the key.

## 7. Revocation

`borg grant revoke <key>` revokes two ways: the next organisation token
leaves the grant out of the roll, and a mined sweep spends its revocation
outpoint, so every BRC-52 verifier sees it revoked and every host retracts
it. A revoked member still in a group is rotation debt until the group's
next change removes them. Revocation is as fresh as the node you ask: a
reader with no node cannot tell a revoked member from a current one, and
borg then says so rather than guess.

## 8. Plane or unicast

On the multicast plane (`mode = plane`, `facade`) each carrier is
submitted once and every subscribed host receives it; tokens and sweeps
also go to every host named. Without it (`mode = unicast`) every object
goes to each host; a carrier counts once `quorum` hosts took it, and a
token or sweep only once every host did. A host that was down misses what
was sent meanwhile in unicast; the next change confirms its tokens and
offers them again.

## 9. Priced questions and the payee

The free questions answer everything a member needs. A host may price the
bulk walk of a chain: `borg history -chain <org|group id>` pays its 402
from your home's coin (at most `-max-sats`). A host's operator settles
what it was paid with `borg payee settle <payments.jsonl>` from the payee's
home, on a schedule.

## 10. Troubleshooting

| You see | It means |
| --- | --- |
| exit 3, `DISAGREES` | a host lacks a token another answered: it is behind or withholding. The answer used the hosts that hold the chain |
| exit 3, `an older state` | the hosts answer an older chain than your home verified: ask other hosts |
| exit 1, `CANNOT OPEN ... removed at seq` | you are not a member of the current epoch |
| exit 3, `no host answers a wrap` | the organisation has not published your wrap yet; ask again shortly |
| exit 2, `quorum` | in unicast a host did not take an object; it is kept and the next command sends it |
| exit 1, `lost` | two of the organisation's devices changed one group at once; the other change won, and this one was discarded without releasing anything. Run it again |
| `ROTATION DEBT` in doctor | a member's grant is no longer current; `borg epoch rotate <group>` removes them now |

## 11. Where to go next

[usage.md](usage.md) for every command, setting and exit code;
[limits.md](limits.md) for every default and cap; [spec.md](spec.md) for
the records; [host.md](host.md) to run a host.
