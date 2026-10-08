# Examples

Task-oriented commands for mainnet and testnet. Every command and flag
here is the code's own; keys, txids and suffixes are placeholders. Output
lines are abbreviated with `…`. The concepts are in
[user-guide.md](user-guide.md), every setting in
[configuration.md](configuration.md).

## 1. What you need

| For | You need |
| --- | --- |
| reading as a member | overlay hosts that carry the hall; nothing else: headers and the chain view default to WhatsOnChain |
| running an organization | the above, and about 20,000 sat in a wallet you control; broadcasting defaults to GorillaPool's public arcade |
| hosts | the `borg-host` image or the module on a reference overlay host ([host.md](host.md)) |

No node is needed. On mainnet and testnet borg reads headers,
transactions, proofs and spends from WhatsOnChain and broadcasts through
arcade, each checked as [configuration.md](configuration.md#no-node-needed)
describes; a node of your own (`chain = asset:<URL>`) is the better option
where you have one.

There is no coinbase on mainnet or testnet for you to mine: a home is
funded by a payment you send from your own wallet. `borg fund` without
`-txid` or `-beef` is for a regtest chain you run (coinbase: only on a
regtest chain you run, for development and tests).

## 2. Check the network from your machine

Reads only, needs no coin and no hosts:

```console
$ borg -network main doctor
home        /home/you/.borg
network     main
hall        not configured
chain       woc:main
…
headers     woc:main tip 970082
fee         100/1000 sat/bytes, floor 100 sat (static)
…
settle      arcade:main
```

On testnet:

```console
$ borg -network test doctor | grep -E 'chain|headers|settle'
chain       woc:test
headers     woc:test tip 1761988
settle      arcade:test
```

A source for the other network is refused before anything is asked:

```console
$ borg -network main -header-url woc:test doctor
borg: header source woc:test is the test network but network is main
$ echo $?
2
```

## 3. Configure an organization's home

```console
$ mkdir -p ~/.borg && cat > ~/.borg/config <<'CONF'
network    = main
mode       = unicast
hosts      = https://host-a.example.com,https://host-b.example.com
CONF
```

For testnet: `network = test`. With a multicast plane: `mode = plane` and
`facade = <the host the plane takes submissions at>`. With a node and a
broadcaster of your own: `chain = asset:<URL>` and `settle =
arcade:<URL>` (or `arc:<URL>`, `rpc:<URL>`).

## 4. Create the identity and fund it

```console
$ borg init
created /home/you/.borg
identity     02c6…09ee5
fund address 1EUY…zipY (main)
```

The identity key is the organization's name in every grant and token.
Back up `~/.borg` as a secret. Send about 20,000 sat from your own wallet to
the fund address (on testnet the address starts with `m` or `n`), then
import the payment one of two ways.

After one confirmation, by its txid:

```console
$ borg fund -txid 9ade…7f2e
imported 1 of 1 output(s) paying 1EUY…zipY, 20000 sat, mined at height 970090; pool 1 output(s), 20000 sat
```

`fund -txid` reads the transaction and its proof from the chain view
(WhatsOnChain by default), checks the proof against the header source, and
adds every output paying the fund address to the pool.

Or at once, from the BEEF your wallet handed over (a file, or `-` for
standard input), with no lookup at all:

```console
$ borg fund -beef payment.beef
imported 1 of 1 output(s) paying 1EUY…zipY, 20000 sat, from 9ade…7f2e, not mined yet: held back until it mines; pool 1 output(s), 20000 sat
```

A payment not mined yet is taken when every parent it spends is proven and
it verifies against them; it is also sent to the broadcaster, in case your
wallet did not, and spent only once it mines. `-refuse-unmined` (or
`fund_unmined = refuse`) takes only a mined one.

A payment that pays the fund address nothing is refused:

```console
$ borg fund -txid 3281…2d86
borg: the payment pays nothing to the fund address 1EUY…zipY
$ echo $?
2
```

On mainnet, `fund` without `-txid` or `-beef` is refused:

```console
$ borg fund
borg: fund without -txid or -beef mines coinbase, which only a chain you run does; on network main, import a payment with fund -txid or fund -beef
```

## 5. Create a hall and give it to your hosts

```console
$ borg hall new example
hall  example_qzxkvbmwtr
topic tm_borg_example_qzxkvbmwtr
host  BORG_HALLS=example_qzxkvbmwtr
$ echo 'hall = example_qzxkvbmwtr' >> ~/.borg/config
```

Each host adds the hall to `BORG_HALLS` ([host.md](host.md)). Then:

```console
$ borg doctor
…
host        https://host-a.example.com answering
host        https://host-b.example.com answering
```

## 6. Create the organization and a group

Each member runs `borg init` (no coin needed) and sends you their
`identity` line.

```console
$ borg org create 'Example' -member $ALICE -member $BOB -member $CAROL
org     02c6…9a1e
hall    example_qzxkvbmwtr
name    Example
seq     0 8b0e…12fa
grant   1c7d…0b3e 03a1…77c2 member
…
$ borg group create engineering -member $ALICE -member $BOB -member $CAROL
group   5a0c4d…9d12
name    engineering
seq     0 3f9a…c410 epoch 0, 3 member(s), wrapped to each
```

Each takes about a block and a minute: the token is mined, published 60 s
later, then the key is wrapped to each member.

## 7. As a member: open the group's key

A member's config:

```text
network    = main
hall       = example_qzxkvbmwtr
org        = 02c6…9a1e
hosts      = https://host-a.example.com,https://host-b.example.com
```

```console
$ borg open engineering
group   5a0c4d…9d12 engineering
latest  seq 0 3f9a…c410 epoch 0, 3 member(s)
epoch   0 OPENED from https://host-a.example.com (commitment 2848…)
$ borg open engineering -write      # before keying new content: is this still the latest epoch?
$ borg member $ALICE                # a key's grants, judged
$ borg member-of $ALICE             # the groups that include a key
$ borg roll                         # every grant, the roll's root compared at each host
```

## 8. Change membership

```console
$ borg org add $DAVE -group engineering        # grants Dave first if needed; keeps the epoch
$ borg org remove $BOB -group engineering      # a new epoch, wrapped to the others only
$ borg org rekey $ALICE $ALICE_NEW             # a key change: a new epoch in every group
$ borg org remove $CAROL                       # offboard: every grant revoked, every group left
$ borg epoch rotate engineering                # a new key, same members
```

Bob, after his removal:

```console
$ borg open engineering
epoch   0 held (commitment 2848…)
epoch   1 CANNOT OPEN: removed at seq 1 (af36…6acd), which began epoch 1; no wrap of a later epoch names this key
$ echo $?
1
```

## 9. Grants: issue, revoke, expiry

```console
$ borg grant issue $ERIN -role engineer -expiry 8760h
$ borg grant revoke $ERIN                 # by the roll and by sweep
$ borg grant revoke $ERIN -by sweep       # or one of the two
$ borg org due                            # removals owed, and grants expiring within 720h (exit 3 when one is due)
$ borg org due -apply                     # make the removals due now; run from a timer
```

## 10. Hosts that disagree: fill

Anyone who reads the hall can repair a host that missed something. No coin,
no key:

```console
$ borg fill
filled  group 5a0c… seq 5 3f9ac4107d21 at https://host-b.example.com
filled  wrap 9ade7f2e0b15 at https://host-b.example.com
filled  2 object(s) at 1 host(s)
$ borg fill
filled  0 object(s) at 0 host(s)
```

## 11. A second home of the organization

On the first home, once:

```console
$ borg org signer
```

On the new machine, with the first home's `identity.json` carried by hand
and the same config:

```console
$ borg init -from identity.json
$ borg fund -txid <a payment you sent to this home's fund address>
$ borg org sync
sync    took 2 organisation token(s), 4 group token(s) (1 new group(s)), 3 grant(s), 2 epoch key(s)
```

## 12. The priced question and the payee

A host may price the bulk walk of a chain. The member's home needs a little
coin (funded the same way as in step 4) and `history_host`:

```console
$ borg terms https://terms.host-a.example.com
service ls_borg, terms 1
history        5 sat a question
history-after  5 sat a question
$ borg history -chain $GROUP -max-sats 10
paid 5 sat to 0268…a450 in 9ade…7f2e, recorded by the host for its payee to settle
seq 0  f635…33e6  height 912406  epoch 0  +3 -0  3 member(s)
seq 1  af36…6acd  height 912408  epoch 1  +0 -1  2 member(s)
```

The host's payee settles what it was paid, from its own home:

```console
$ borg payee key -out payee.env        # BORG_PAYEE_KEY for the host's environment
$ borg payee settle /var/lib/borg/payments.jsonl
1 payment(s) settled, 5 sat; 0 settled before; 0 not settled; 0 refused (0 before); pool 1 output(s), 5 sat
```

## 13. The kill switch

```console
$ borg kill -confirm KILL           # lists the sweeps; sends nothing
$ borg kill -confirm KILL -yes      # revokes the whole roll, one sweep per tree
```

## 14. In a container

```console
$ docker run --rm -v borg-home:/home/nonroot/.borg \
    -e BORG_NETWORK=main \
    ghcr.io/lightwebinc/borg init
$ docker run --rm -v borg-home:/home/nonroot/.borg \
    -e BORG_NETWORK=main \
    ghcr.io/lightwebinc/borg doctor
```

## 15. Exit codes in scripts

| Code | Meaning |
| --- | --- |
| 0 | done, or verified |
| 1 | refused: the evidence contradicts what was asked |
| 2 | usage, configuration or transport error; persisted work is finished by the next command |
| 3 | incomplete: hosts disagree or did not answer; proves nothing either way |

```console
$ borg org due >/dev/null || [ $? -ne 3 ] || borg org due -apply   # from a timer: apply only when a removal is due
```
