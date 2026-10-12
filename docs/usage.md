# The borg command

`borg` runs an organization and its groups ([spec.md](spec.md)), and reads
them as a member or as anyone. The organization's home holds its identity
key: it issues each member a BRC-52 grant, keeps the roll on a chain of
mined tokens, and creates groups whose every removal begins a new epoch
with a fresh key wrapped only to the members who remain. A member's home
opens the epoch keys its key can open, and says which it cannot. Anyone
reads the roll, a membership or a group from one or several hosts and
checks every answer against block headers it chooses. A reader pays a
host's 402 for the priced history question, and a host's payee settles
the payments the host recorded.

```text
  organization home                                       overlay hosts (tm_borg_<hall>, ls_borg)
  ─────────────────                                       ──────────────────────────────────────
  init ─▶ fund ─▶ org create ─▶ grant carriers ──────────▶ facade (plane) or each host (unicast)
                  │             org token seq 0, mined ─▶ facade and every host named
                  │  └─ chain, grant, record trees, mined first ─▶ settlement leg ─▶ chain
                  ├─ group create, org add/remove/rekey/due, epoch rotate
                  │       group token, mined ─▶ facade and every host named
                  │       wrap carriers (the epoch key to each member) ─▶ facade or each host
                  ├─ grant revoke ─▶ org token (roll) and sweep, mined ─▶ facade and every host
                  └─ org sync ◀── every host: chains, roll, the wraps to the signer key
  member home
  ───────────
  open <group> ◀── ls_borg answers of every host, compared, checked against header_url
               ─▶ the epoch keys this key can open, kept in the home
  open -write  ◀── the node's view: is the group's latest token unspent?
  anyone
  ──────
  roll, member, member-of, group show ◀── every host, compared, checked against header_url
  history ──BRC-104, 402, BRC-29 payment──▶ the host's terms route (history_host)
  host's payee
  ────────────
  payee settle payments.jsonl ─▶ each payment: checked, broadcast, pooled
```

[user-guide.md](user-guide.md) explains the concepts and the day-to-day
use; this page is the reference.

## Install

```console
$ go install github.com/lightwebinc/borg/cmd/borg@latest
$ make borg              # or from a checkout: bin/borg (Go 1.27.2 or later)
$ make docker-build      # ghcr.io/lightwebinc/borg and borg-devchain, locally
```

Release tarballs for Linux and macOS (amd64, arm64) are on the
[releases page](https://github.com/lightwebinc/borg/releases).

In the image the home is `/home/nonroot/.borg`; mount a named volume there
(`-v borg-home:/home/nonroot/.borg`) and pass settings as `BORG_<KEY>`
variables.

## Commands

| Command | Needs a home | Needs | What it does |
| --- | --- | --- | --- |
| `init [-from FILE]` | creates it | nothing | identity key and empty coin pool; prints the identity and the fund address. `-from` makes the home around another home's `identity.json`: an organization's second home |
| `fund -txid TXID` | yes | `chain`, `header_url` (defaults on main and test) | imports a mined payment you sent from your own wallet to the fund address, read from the chain view, once its proof checks. How a home is funded on mainnet and testnet |
| `fund -beef FILE\|- [-refuse-unmined]` | yes | `header_url` | imports that payment from the BEEF your wallet handed over, with no lookup: a mined one once its proof checks, an unmined one (unless `-refuse-unmined` or `fund_unmined = refuse`) once its parents' proofs check and it verifies against them, held back until it mines and sent to `settle` in case the wallet did not |
| `fund [-blocks N] [-batch N] [-rescan]` | yes | `rpc`, `chain = asset:<URL>` | coinbase: only on a regtest chain you run (development and tests). Mines coinbase to the fund address through `generatetoaddress`; `-rescan` re-reads recent blocks for coinbase the pool lacks. Refused on network `main` |
| `hall new <name>` | optional | nothing | draws the 10-letter random suffix; prints the hall, its topic, and the host's `BORG_HALLS` line |
| `hall list` | yes | nothing | the halls this home created |
| `org create <name>` | yes, funded | `hall`, a publisher's settings (below) | begins the organization: a chain tree and a grant tree, a grant to each `-member`, and the first organization token admitting them |
| `org show` | yes | nothing | the organization's own record: its chain, every grant and its state, every group and its members, rotation debt marked |
| `org add <key>... -group G` | yes, the organization's | a publisher's | grants each key that holds no current grant, admits them in one organization token, and adds them to the group in one group token |
| `org remove <key>... [-group G]` | yes, the organization's | a publisher's | with `-group`, a removal from the group (a new epoch); without, offboarding: every grant revoked by the roll and by sweep, and a removal from every group that holds the keys |
| `org rekey <old> <new>` | yes, the organization's | a publisher's | a member's key change: a grant to the new key, the old key's grants revoked (roll and sweep), and in every group a transition that removes the old key and adds the new one (a new epoch) |
| `org signer` | yes, the organization's | a publisher's | grants the organization's signer key and adds it to every group that lacks it, so a second home can open each epoch's key |
| `org sync [-retire-trees]` | yes, funded to go on | a publisher's, `hall` | rebuilds the home's record of the organization from the hosts: both kinds of chain, the roll, and each epoch key from the wrap to the signer key. Exit 3 when the current epoch's key of a group is not open here, or a host answered something that does not verify (named, and not taken) |
| `org due [-within DUR] [-apply]` | yes, the organization's | nothing; with `-apply` a publisher's | lists the removals due (group members whose grant is no longer current; exit 3 when there is one) and the grants of group members expiring within `-within` (default 720h); `-apply` makes the removals due now |
| `grant issue <key>` | yes, the organization's | a publisher's | issues a grant (`-role`, `-scope`, `-admin`, `-expiry`) and admits it to the roll |
| `grant revoke <key\|txid>...` | yes, the organization's | a publisher's | revokes by the roll and by sweep, or one of them (`-by roll`, `-by sweep`) |
| `role define <name>` | yes, the organization's | a publisher's | publishes a role and the permissions it carries (`-permission`, given once per permission), and the organization's next token defines it. Redefining a role the set already defines retires and sweeps the definition it replaces in the same transition, so the set never holds two |
| `role retire <name>` | yes, the organization's | a publisher's | both halves of a withdrawal: a token drops the definition from the policy set and a sweep retracts its carrier. `-by set` or `-by sweep` does one of them. The bindings that name it stay and authorize nothing |
| `role list` | optional | nothing | the roles the policy set defines, with their permissions |
| `bind <role> -to <group\|key>` | yes, the organization's | a publisher's | gives the role to a group identifier or to one identity key; `-only` keeps part of it and can never widen it, `-expiry` ends it. A binding carries a role's name, not its permissions, so binding a role the set does not define says so and authorizes nothing until it does |
| `unbind <txid>` | yes, the organization's | a publisher's | retires one binding by its carrier's txid, the same two halves and the same `-by` |
| `policy [show]` | optional | a reader's settings | the organization's policy set from the hosts, every record verified and each host's answer held to the root the latest token commits to. `-role R` asks for one role's definition and the bindings that give it, `-bound X` for the bindings naming one group or key, each answered with a path into the policy root. `-local` reads this home's own record instead |
| `policy check <key> <permission>` | optional | a reader's settings | decides one permission as a service does (spec section 13): the subject's current grants, the groups that hold it and the policy set, each from the hosts and checked against this machine's own headers, folded and matched. It fails closed and prints the state it decided over. `-local` uses this home's own record |
| `group create <name>` | yes, the organization's | a publisher's | a group of `-member` keys (each with a current grant), epoch 0 wrapped to each; `-rotate-on-join`, `-rotate-every` |
| `group list` | optional | nothing, or a reader's settings with `-org` | the organization's groups: the home's own record, or another organization's from the hosts |
| `group show <group>` | optional | a reader's settings | the group's chain folded from seq 0, and its members enumerated from the current epoch's wraps; stray and incomplete wraps reported |
| `epoch rotate <group>` | yes, the organization's | a publisher's | a new epoch with the same members (rotation debt removed first) |
| `kill -confirm KILL [-yes]` | yes, the organization's | with `-yes` a publisher's | the kill switch: one sweep per grant tree and record tree the home holds. Every grant revoked, every grant and wrap retracted. Without `-yes` it lists the sweeps and sends nothing. Work an earlier command left unfinished does not stop it |
| `open <group>` | yes | a reader's settings | opens every epoch key this home's key can open, keeps each, and says which it cannot and why; `-write` adds the writer's evidence; `-show-key` prints the current key |
| `roll` | optional | a reader's settings | the roll from every host, every grant verified and judged, the root of each host's answer compared with the latest token's |
| `member <key>` | optional | a reader's settings | whether the key is a current member, and its grants; for this home's own key, each grant is kept whole in the home and opened through its keyring |
| `member-of <key>` | optional | a reader's settings | the groups whose members include the key, each group's chain folded here |
| `history` | yes, funded | a reader's settings, `history_host`, `chain`, `settle` | the priced question: pays the host's 402 from the pool and prints every token of a chain (`-chain org` or a group id), one page, or every page with `-all`. One payment a question, at most `-max-sats`, and at most `-budget` in all |
| `fill [group...]` | optional | a reader's settings, `chain` | replays to each host what another host answered and it lacks: the organization's chain, the roll's grants, the policy set's records, and each group's chain and wraps (spec section 12); signs nothing |
| `terms [URL]` | no | `history_host` or URL | prints a host's terms document |
| `payee key -out FILE` | yes | nothing | writes `BORG_PAYEE_KEY` for this home's identity to a new file, mode 0600 |
| `payee settle <payments.jsonl>...` | yes, the payee's | `header_url`, `chain`, `settle` | internalizes every payment in a host's ledger this home has not: all broadcast, then awaited together (`-in-flight`) |
| `doctor` | optional | nothing | the home's state (trees, organization, transition in flight, outbox, rotation debt, bindings that authorize nothing, grants expiring, rotations due, an epoch key not held, unmined payments), the chain view, broadcaster and fee policy in use, and whether the headers, hosts and priced host answer; reads only |
| `version` | no | nothing | prints the version |

A reader's settings are `hosts`, `header_url`, `hall` and `org` (the
organization's identity key; a home that runs an organization reads its
own by default). A publisher's settings are those, and `chain`, `settle`,
and `facade` in mode `plane`. On `main` and `test`, `header_url`, `chain`
and `settle` default to WhatsOnChain and GorillaPool's public arcade, so no
node is needed ([configuration.md](configuration.md#no-node-needed)).
Below, "the node" means the chain view (`chain`), whichever it is. `<key>` is an identity key, 66 lowercase hex
digits. A group is its id (32 lowercase hex digits) or its name; a name
two groups share is refused. A hall is always the full identifier
`<name>_<suffix>`. A txid is 64 lowercase hex digits, in display order.
`borg <command> -h` prints a command's flags.

## Configuration

Settings come from, highest first: the global flags, `BORG_<KEY>` in the
environment, the config file, and the defaults. Every key, flag and
environment variable, with its default, is in
[configuration.md](configuration.md). A reader's settings are `hosts`,
`header_url`, `hall` and `org`; a publisher's are those and `chain`,
`settle`, and `facade` in mode `plane`.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | done, or everything asked verified (the hosts may still disagree about a chain a key was read from; that is reported) |
| 1 | refused: a key this home cannot open (not a member of the current epoch), a paid `history` page that holds a token that does not verify (the one host asked, named), a grant that is not current, a transition another one beat to the chain (`lost`), a catch-up that found a fork, or a sweep or payment the network refuses |
| 2 | usage, configuration, local or transport error, a quorum not met, a price over `-max-sats` or `-budget`, or a transition or sweep a leg refused that may still mine (it is kept); what was persisted is sent or published by the next command |
| 3 | incomplete: the hosts disagree, a host answered something that does not verify (it is named and not used), a host could not be asked, no host answered what was asked, a host answers an older state than this home verified, verified tokens exist above the chain read (`WITHHELD`), a budget ran out before a chain was read to its end, no spend source could give positive evidence, a removal is due (`org due`), or a catch-up could not reproduce the roll or open the current epoch's key. It proves nothing either way |

## A home

`init` creates the home at mode 0700: `identity.json` (the identity key),
`wallet.json` (the coin pool) and, once anything is done, `state.json`.
The state holds what must survive a crash: the three kinds of funding tree,
the organization's chain and grants and groups, every epoch key the
organization sampled, every object persisted and not yet confirmed
published, the sweeps built, the payments made and not yet mined, and, for
a member, the epoch keys it opened, the grants to it it read (whole: the
certificate, never a summary), and the latest token of each chain it
verified. **The home holds secrets**: the
identity key and every epoch key. Back it up as one, and keep it at 0700.
One command at a time uses a home; a second is refused (exit 2). The lock
is taken before the wallet is opened: the coin pool is read whole and
written back whole, so a pool read before the lock could put back coins
another command spent.

A coin leaves `wallet.json` the moment it is taken, before the transaction
that spends it is recorded in `state.json`. While a command that spends is
running, the state records the pool as it stood (`taking`); a command that
ends clears it. The next command after one that stopped in between asks
the node about a coin in that record that neither file accounts for: one
the node shows unspent goes back in the pool, one it shows spent is
reported, and one it cannot answer for is looked at again next time.

A funding tree has a record of its own. Before a tree of any kind reaches
the settlement leg, the state records it with its kind and the coin it
spends (`pendingTrees`), and the save that records the tree as adopted
drops that record. A command that stopped in between, while the tree was
on its way to the chain or, minted ahead, before the command ended, leaves
the record, and `doctor` names it (`tree ... signed for coin ... and not
recorded as minted`). The next command that publishes asks the node what
became of the tree before it spends anything, and says what it found:

| The node shows | The command does | It says |
| --- | --- | --- |
| the tree, its coin not spent by another transaction, and the current tree of its kind is used up or there is none | adopts the tree and takes its unspent change into the pool | `funding tree ... is recovered` |
| the tree, its coin not spent by another transaction, and the current tree of its kind still has outputs | holds the tree, behind any tree already held, takes its change, and adopts it when the trees before it run out; the record stays until then | `funding tree ... is recovered and waits for the switch` |
| no such tree, and the coin unspent | puts the coin back in the pool and keeps the record for one more command; the second such answer drops it | `... never reached the chain: its fee coin ... is unspent and back in the pool`, then `its record is kept for one more command` |
| no such tree, and the coin spent by another transaction | drops the record | `... never reached the chain: its fee coin ... is spent by ...` |
| the tree, with no proof, and the coin spent by another transaction | drops the record and adopts nothing: the tree lost a double spend and never mines, though the node still serves it | `... lost a double spend: the node still serves it, without a proof, and its fee coin ... is spent by ...` |
| the tree, with no proof, and it cannot say who spent the coin | keeps the record and goes on; the next command asks again | `... could not be settled now (...): its record is kept and the next command asks again` |
| nothing: it cannot be asked, or cannot say | keeps the record and goes on; the next command asks again | `... could not be settled now (...): its record is kept and the next command asks again` |

A tree that reaches the node after its coin went back is found by the next
command, which adopts it and takes the coin, now spent, out of the pool
(`... its fee coin ... is spent and is taken out of the pool`). Several
trees found on the chain while the current tree has outputs are all held,
and are spent one after the other, in the order of their records. A command
that only reads (`roll`, `open`, `member`, `group show`, `history`,
`doctor`) asks nothing about a tree and needs no node for it. What is left
open is in [limits.md](limits.md).

The home pays for its trees from its own pool. A deployment that funded
trees through a BRC-100 wallet instead (bcommon's `Trees.Fund`) would not
be covered by any of this: such a wallet chooses the coins, signs and
broadcasts inside one call, so there is no moment before the broadcast to
record the tree in, and a run that stopped after the broadcast and before
the tree was adopted would leave a tree only that wallet knows.

An organization's and a group's name is one line of plain text: `org
create` and `group create` refuse a name with a line break or a character
a terminal would act on. A name another tool wrote is printed on one line,
filtered.

A home is one identity. An organization is an identity key: the home that
runs `org create` is the organization, and its key signs every grant
(BRC-52's certifier), every token and every wrap. A member's home needs no
coin unless it asks the priced question.

## Halls

`hall new <name>` draws the 10-letter suffix uniformly at random and
prints the hall `<name>_<suffix>`, its topic `tm_borg_<name>_<suffix>`,
and the line a host takes (`BORG_HALLS=<hall>`). Many organizations may
share a hall; an organization's chains and carriers all live in one. The
suffix keeps unrelated halls off each other's topic. It is not a secret
and not an access control.

## The organization

Every change is one or more mined tokens plus carriers (spec section 7).
A transition is built in the home, **persisted with the epoch key it
samples before anything is sent**, settled, and waited for until it is
mined in this home's headers. It took effect only when a transaction
spending its predecessor is mined and carries byte for byte the record
built: a copy of it with another txid (re-encoded unlocking scripts) is
taken as the same transition; any other spender won, and the transition
is discarded with its key (exit 1). Then, after `delay`, the token is
published with its parent, each with its minimal proof, to the facade and
to every host named (in unicast to every host), and confirmed at each
host by a lookup. Every token and sweep must reach every host named; a
carrier needs the quorum.

**A transition or a sweep is given up only on the node's word that
another transaction spends one of its inputs.** A settlement leg's refusal
alone decides nothing: it is also how a second submission of a transaction
the network already took is answered. While the node shows the
predecessor (and the fee coin) unspent, spent by this very transaction, or
cannot say, the transition stays persisted with its key and the next
command sends it and waits again (exit 2). The same holds for a spender
the node names and cannot show: it may be this transition under another
txid, so nothing is discarded until it is read. A sweep is marked failed,
and its fee coin given back, on the same evidence only.

**Proofs after a reorganization.** The proof a home keeps of a token or a
funding tree is the one the node gave when it mined. Before a token is
built on, published as a parent, or a tree's output is spent, the kept
proof is checked against `header_url`; one that no longer verifies is
replaced by the transaction's current proof from `chain`, in the home too
(spec section 11.5). A header source that fails is an error, never a
verdict.

**Unfinished work.** Every engine command first finishes what the home
persisted: a sweep, the outbox, a transition, then the tokens and wraps it
owes. Each is tried whatever became of the one before, so one object a
host will not take does not keep a mined transition from being adopted or
a token from the hosts. The command then stops on the first failure
(exit 2), except `kill`, which goes on.

The tooling refuses to build what a host would refuse or an index would
report: every transition is checked against the host's transition rules
and folded over the organization's own chain first, and one that would be
reported `roll-mismatch`, `members-mismatch`, `commitment-reuse` or
`rejoin-unrotated` is not built.

A group whose current epoch already holds a token the index reports
`commitment-reuse` or `rejoin-unrotated` (another tool built it; this one
does not) stays the organization's to run: `doctor` names it
(`INCONSISTENT`), the epoch's key is wrapped to nobody else, and every
transition of the group begins a new epoch, which is the remedy (`borg
epoch rotate <group>`). A chain that folds to a mismatch is not operable.

**Rotation.** A transition begins a new epoch, with a key sampled fresh
(never derived), when it removes anyone, when it adds a key any earlier
token of the group removed, when the predecessor sets `rotateOnJoin` and
it adds anyone, when it adds anyone from a home that does not hold the
current epoch's key (another home began the epoch), and on `epoch rotate`.
A key change removes the old key, so it rotates too. Every group transition first removes the group's
**rotation debt**: members whose grant is no longer current by the
organization's own record (revoked in the roll, swept, or expired).

**a binding that authorizes nothing**: a binding in the policy set naming
a role the set does not define, bound to a key with no current membership
grant or to a group this organization does not hold, or whose expiry has
passed. The fold ignores it, so it grants nothing and is harmless; `doctor`
names it because a reader of the set would otherwise take it for authority
someone holds. `borg unbind <txid>` clears it.

**Wraps.** Once a group token is mined (and one further block when
`signers` is above one), the epoch key is wrapped to each member who has
none yet: with a new epoch to every member, otherwise to the members just
added, in records of up to `entries`, each on its own record-tree output.
A wrap is made only for a key with a current grant. A command that stops
before making them makes them next time: every engine command finishes
what the home owes first.

**Grants.** A grant is published before the organization token that
admits it. No free class answers a grant before the roll holds it, so a
host's answer that admits nothing for a new grant is counted,
and once the admitting token is published each host is asked for it by
the `member` class and offered it again where it lacks it. A grant fewer
hosts than the `quorum` then answer is not published: it goes back to the
outbox (exit 2), and the next command publishes it.

**Revocation.** `grant revoke` revokes by the roll (an organization token
revoking the grants' commitments) and by a mined sweep of their
revocation outpoints (each grant-tree output its carrier spent), which
every BRC-52 verifier sees as revoked and every host as retracted. One
sweep per grant tree. A sweep is not erasure.

**Removals due.** A grant that expires, or is revoked while its holder is
still in a group, leaves a removal owed. `org due` lists them from the
home's own record, with the grants of group members that expire within
`-within`, and exits 3 when a removal is due now. `org due -apply` makes
each: one transition per group that holds such a member, which removes
them and begins a new epoch. Nothing is removed before its grant stops
being current, so run it from a timer; a grant renewed in time (`grant
issue` to the same key) is never due. `doctor` shows the same debt and
counts the grants expiring within 30 days.

## Roles and policy

A grant says who a member is. A role says what its holders may do, and the
organization's own chain commits to both, so a service never holds a new
rule against an old membership.

```text
$ borg role define releaser -permission bsecret/release/payroll -permission 'bstore/get/*'
role    <txid> releaser bsecret/release/payroll bstore/get/*
org     seq 4 <txid> defines it
$ borg bind releaser -to <group id>
binding <txid> releaser
org     seq 5 <txid> defines it
$ borg policy check <member key> bsecret/release/payroll
ALLOW   <member key> bsecret/release/payroll
holds   bsecret/release/payroll bstore/get/*
```

A permission is `service/action/resource`: the service names itself, and
its own specification fixes what its actions and resources mean. A resource
may end in `*`, which makes it a prefix; nothing else is a wildcard, and a
service never asks with one.

A subject holds the union of the role its grant names and every binding
that reaches it, through a group or its own key. Nothing subtracts: a
permission is taken away by retiring the record that carries it, by
revoking the subject's grant, by removing the subject from the group, or by
letting the binding expire. Each of those is a mined change.

`policy check` answers from this home's own record of the organization. A
service decides for itself, from a host's answers checked against its own
block headers; what it decided and the state it decided against is what a
decision receipt records.

## Several homes of one organization

An organization is an identity key, and any home made around that key
(`init -from <identity.json>`) can run it. What such a home lacks is at the
hosts, and `org sync` reads it as a reader does (spec section 7.4):

1. the organization's chain and every group's, each token verified against
   `header_url` and folded from seq 0. Tokens the home lacks are taken; a
   token of the home's that the chain no longer holds (a reorganization) is
   dropped with what followed it; two mined tokens at one seq is a fork,
   and nothing is merged (exit 1);
2. the roll: every grant, from every host, until the root of the latest
   token is reproduced. A roll no host answers whole is not taken in part
   (exit 3), because a grant the home lacked would read as a member owed a
   removal. An opaque grant's fields are opened through its keyring;
3. each epoch key, from the wrap to the **signer key**, checked against the
   epoch's commitment as a member checks it. The signer key is the BRC-42
   child of the organization's key under `[1, "borg organisation"]`, key id
   `signer`, counterparty anyone: every home of the organization derives
   it, and anyone can tell from the organization's key which member it is.
   `org signer` grants it and adds it to every group; with `signers` above
   one every group the home creates or changes includes it. An epoch it
   was not a member of is printed as `missing`; while that is a group's
   current epoch the home wraps it to nobody (exit 3), and an addition it
   makes to the group begins a new epoch instead;
4. the coins and the trees. A coin the node shows spent (another home held
   it too) leaves the pool; the change of each token taken joins it. A
   grant or record tree another home also spends is retired here, and
   `-retire-trees` retires them all (for a home that was copied). The tree
   of every grant and wrap the hosts answer is recorded, so this home can
   sweep it.

With `signers` above one a command does this by itself, before it builds
anything, whenever the node shows one of the home's latest tokens spent or
a host lists a group the home lacks, and after a transition of its own
lost. A transition that loses (exit 1) released nothing: wraps follow
their token by a block.

Not recoverable from the hosts: the key of an epoch the signer key was not
a member of; a grant issued and not yet admitted; another home's outbox,
sweeps in flight and funding trees with outputs left. The epoch key of a
transition that lost is discarded by the home that sampled it. Two homes
must not spend one funding tree: of two carriers on one funding output a
host answers one. A home made with `init -from` mints its own; a copied
home shares them until `org sync -retire-trees`.

## The kill switch

`kill -confirm KILL` lists, and with `-yes` makes, one sweep per grant
tree and per record tree the home holds, each spending every output of the
tree, used or not (spec sections 9.4 and 9.5). Each is persisted, settled,
waited for until it mines (one block a tree), and published after `delay`
to the facade and to every host named. Once they mine, every grant those
trees funded is revoked for every BRC-52 verifier, and every honest host
retracts the grants and the wraps. The home's trees are then spent: a
later grant mints a new one. A run that stops is finished by running it
again.

Work an earlier command left unfinished (an object a host has not taken,
a transition not yet mined) does not stop it: that work is reported and
left for the next command. Before a tree is swept the node is asked what
spends each output: one another transaction already spends is left out,
and once that transaction is mined it is recorded as the sweep it is and
published as one (another home of the organization swept it, or an
earlier run gave up on a sweep that mined all the same). An output whose
spender has not mined yet is left for the next run (exit 3). A sweep
refused because another transaction took one of its inputs is built again
without it, twice at most.

It does not end the chains: tokens are mined and stay, and readers go on
reading them with every member lacking a current grant. It does not take
back an epoch key a member opened, or a copy anyone kept. It reaches the
carriers of another home only once this home knows their trees (`org
sync`).

## Reading

Every reading command asks every host in `hosts` and checks every answer
against `header_url`: a token by its record, its lock and field signature
and its proof; a carrier by the host's own admission rules and SPV through
its funding parent; a sweep by its proof. A proof a host stored that no
longer verifies (a reorganization mined the transaction again elsewhere)
is replaced by the transaction's current proof from `chain`, when one is
configured, and verified against `header_url` like any other; without
`chain` such an answer is refused as unmined. A chain is read by asking
each host the latest class and then every seq from 0, so the reader folds
it itself: the canonical chain runs by spends from the first-mined seq 0
token, and a host that lacks a token another host answered is reported
(`DISAGREES`), never merged silently. A seq is asked for only once the one
before it is on the chain, so the walk is bounded by the chain, never by a
seq a host claims. Verified tokens the hosts answer above where the walk
ended are reported (`WITHHELD`, exit 3): a token between was withheld, or
the organization signed a stray one, and what was read may be an older
state. A chain that folds to a mismatch is
unusable from that token on (fail closed); `commitment-reuse` and
`rejoin-unrotated` are shown as inconsistent.

A host that answers a token, a grant or a wrap that does not verify (bytes
substituted or damaged on the way, an object that breaks the host rules,
a wrap of another epoch than the one asked, more outputs to one question
than a page holds) is named (`REFUSED`), what it
answered is not used, and the command is incomplete (exit 3): what is read
comes from what verified, and one host at fault does not make a check
fail. When every token the hosts answered is refused, the command names
each one and the header source it was checked against, since a header
source that does not answer fails every answer the same way; it never
says that no host answers.

A home remembers the latest token of each chain it verified, and refuses
(exit 3) a host set that answers an older one: a host can withhold a
newer token, and only a reader that remembers can tell. `open` reads
through that memory; the commands that need no home (`roll`, `member`,
`member-of`, `group show`) have none, and rely on `WITHHELD` and on asking
more than one host.

**`open <group>`** applies the member's checks of spec section 6.4 to
every epoch: the token that began it (from the canonical chain), a wrap
that names this key, the wallet's decryption, the commitment. A key that
opens is kept in the home. An epoch this key was never a member of is
named without asking. Every host is asked for the wrap, after one has
opened the key, so a host that answers a wrap that does not verify is
named beside an honest one (exit 3). The current epoch's key not opening
is exit 1, with the token that removed the key; no host yet answering its
wrap is exit 3, and so is a key that was a member of the epoch by the
group's own chain when every wrap a host answered for it was refused: that
is an absence, and the hosts are named.

**`member <key>`** judges each grant: in the roll of the latest usable
token (its inclusion path checked, else the roll folded here), a scope
that grants `member`, not expired (300 s of skew allowed), revealed unless
the organization is opaque, and its revocation outpoint unspent per
`chain`. Without a spend source a grant is never current (exit 3).

## Filling a host

A host that was down off the plane, or was left out of a unicast publish,
lacks what the others hold, and nothing resends it: `roll`, `group show`
and `open` report it (`DISAGREES`). `fill` repairs it from the other hosts'
answers (spec sections 11.6 and 12). Each token a host lacks is replayed in
the BEEF a host admits, assembled from the token and its parent (the token
at `seq - 1`, or at `seq` 0 the chain tree with its proof); each grant of the roll and each wrap of a group, of every epoch
some host still answers, is submitted as another host answered it. Everything is verified against
`header_url` first, and each fill is confirmed by a lookup at the host it
went to. Anyone who reads the hall can run it; it needs no coin.

The chain tree at `seq` 0 is the one part no host keeps. Run in the
organization's home, `fill` takes it from the home's own record of its
funding trees, which keeps every tree it minted with its proof, and asks
`chain` only when the home holds no record of it. Elsewhere it asks
`chain`, which may have dropped the tree: a node can prune a transaction
whose outputs are all spent, and each chain tree output is spent by a
chain's first token. A tree taken from the home is checked as the node's
answer would be: its txid must be the one the token spends, its proof must
verify against `header_url` (a proof a reorganization left stale is
replaced by the node's current one when the node gives one), and the
token's BEEF must pass the host's rules. A record whose bytes are another
transaction is named (`REFUSED`) and not used. A second home records each
chain tree, with its proof, when `org sync` first takes a chain, while the
node still answers it; a home made by `org sync` before borg v0.1.6 holds
no such record. When neither the home nor the node supplies the tree,
`fill` names it and exits 3:

```text
organisation chain seq 0 <txid>: cannot assemble its BEEF: its chain tree <txid>: this home keeps no record of it (a home made by org sync before borg v0.1.6 does not), and the node does not answer it with a proof that makes the token's BEEF (a node drops a fully spent transaction, and no host keeps a chain tree); run borg fill from the organisation's home that published the chain
```

From a home that is not the organization's, the message says `this home is
not the organisation's` in place of the first clause.

On the plane the organization's own tooling does part of this as it
publishes: an object is submitted once to the facade, then looked for at
every host in `hosts` (for up to 3.5 s), and offered directly to a host
the plane did not deliver it to. The facade's answer is not a host's: it
says "nothing admitted" for a duplicate and for a refusal alike. So a
wrap counts as published only once `quorum` hosts answer it, and a token
or a sweep once every host does; until then it stays in the outbox (exit
2) and the next command publishes it. Grants are confirmed the same way
once the roll admits them.

## The spend index

A writer must key new content only under the epoch of the group's latest
token, with positive evidence at most `freshness` old (spec section 6.5).
`open -write` gathers it from three places:

1. **the home's own view.** The latest token this home verified for the
   group; a host set answering an older one is refused (above). When the
   home runs the organization and holds its own transition of the group in
   flight that begins an epoch, writers wait: the home knows the key is
   about to change.
2. **the proof.** The latest token of the canonical chain verifies against
   this home's headers.
3. **the node's spent-output check.** `chain`'s view of the token's
   1-satoshi output (bcommon's `nodeapi`): unspent is the evidence. Spent
   by a transaction not yet mined, that transaction is read from the node:
   the group's next token keeping the epoch lets the writer go on; one
   beginning an epoch, or anything else (which terminates the chain), is no
   evidence. Only a positive "unspent" counts: a node's status `OK`, or
   WhatsOnChain's "known output, no spend" (404). A source that cannot
   answer, or answers anything else (a node's `NOT_FOUND`, which it also
   answers for an output it pruned or could not read, `IMMATURE`,
   `FROZEN`, a `SPENT` naming no spender; WhatsOnChain's "unknown output"
   (400)), is no evidence, and the writer fails closed (exit 3).

Evidence is recorded in the home with its time and source, and reused
while it is younger than `freshness` and names the same token. The same
node answers the revocation check of `member` and `roll`. Asking a third
party (WhatsOnChain, the default) every minute tells it which groups a
writer writes to; a writer's own node keeps that to itself (spec section
16.12).

## The priced question

`history` asks the host's terms route (`history_host`, or `-at`) over
BRC-104 for every token of a chain (`-chain org` or a group id, `-after
SEQ` for the next page, `-all` for every page from there on). The host
answers 402 with its price; the command pays it from the pool with a
BRC-29 payment as output 0 (BRC-105, through the SDK's `AuthFetch`),
unless the price is over `-max-sats` (default 1000), and verifies every
token answered.

**One question is paid for once.** The SDK answers every 402 with a new
payment; the command does not. A host that answers 402 again after a
payment was sent is refused (exit 1), and handed nothing more. The
payment is recorded in the home before it is sent, and from then on it is
made: the host holds a valid transaction that spends the coin. A host
that takes it and does not answer (a 500, a dropped connection, a 429 on
the paid request) is reported, the payment is kept and broadcast, and its
coin is never given back to the pool, where the next payment would spend
it a second time.

**A budget for the whole command.** `-max-sats` bounds one question,
`-budget` (default 16000) everything the command pays. With `-all` each
page is one paid question; when the budget cannot cover the next page the
command stops and says the chain was not read to its end (exit 3). A page
holds at most 64 tokens, an answer at most 16 MiB, and a page that holds
tokens at or below the cursor it was asked after is refused. A page is one
host's paid answer, with no other host beside it: a token in it that does
not verify is refused (exit 1), and the free `org-seq` and `group-seq`
classes read the same tokens from every host.

The payment is this home's own transaction. The host records it and its
payee broadcasts it when it settles; this home also hands it to `settle`
once it was sent, answered or not, so it mines whenever the payee runs. Its
change is spendable by this home's next payment at once: that payment
spends the unmined change and carries the payment it came from in its
BEEF, which the host verifies through. Up to 16 unmined payments may follow
one another this way, so a home with one coin is not held to one priced
question per block; for anything but a payment, the change is spendable
once the payment mines. A coin is handed out once, and only a payment
that was never made (the price was over the bound, or nothing could be
signed) leaves its coin in the pool: no coin is spent twice. `terms`
prints what a host prices; a free class with a price is reported and
never paid.

## A host's payee

`payee key -out FILE` writes this home's identity key as `BORG_PAYEE_KEY`
for a host's environment. `payee settle <payments.jsonl>...` takes every
payment the host recorded and this home has not settled: each checked to
pay the key this identity derives for its remittance and payer, verified
against the headers, broadcast through `settle`, and awaited; every
payment is broadcast before any is waited for (at most `-in-flight`, default
16), so a run takes about one block. A payment that spends the change of
the payer's earlier unmined payment carries that payment, and it is
broadcast first. A payment whose payer spent the inputs
elsewhere is detected from the node's view of its inputs, reported once,
recorded, and passed over afterwards.

## Doctor

`doctor` reads only: the home, its trees, the organization it runs, a
transition in flight, the outbox, failed sweeps, each group's rotation
debt, the members whose grant expires within 30 days, a rotation due by
`rotateEvery`, a group whose current epoch key this home does not hold, the
unmined payments whose change the next payment may spend, and whether the
node, the header source, every host and the priced host answer.
