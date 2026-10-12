# Running a borg host

A borg host is an ordinary overlay host that loads the borg module: one
file, `host/bundle/borg-module.js`, which mounts the topic manager
`tm_borg_<name>_<suffix>` for each hall it carries and the lookup service
`ls_borg` for all of them ([spec.md](spec.md) sections 10 and 11). The
module imports nothing but `@bsv/sdk` (the host's own copy) and Node's
built-ins; bcommon is inlined.

## Build and deploy

```console
$ (cd host && npm ci)   # once
$ make module           # writes host/bundle/borg-module.js
```

Copy the file into a directory beside the host's `node_modules` (for
example `<host>/modules/borg/borg-module.js`) and name it by absolute path:

```sh
OVERLAY_TOPICS=tm_borg_example_hall_vkqzmtrwxp
OVERLAY_MODULES=/srv/overlay/modules/borg/borg-module.js
BORG_HALLS=example_hall_vkqzmtrwxp
BORG_STATE_DIR=/var/lib/borg
BORG_HEADERS_URL=https://headers.example.com
```

Or run the image `ghcr.io/lightwebinc/borg-host` (built by
`make docker-build-host REFERENCE_HOST_IMAGE=<reference overlay host
image>`): the reference host with the module at
`/app/modules/borg/borg-module.js`, its state directory at `/var/lib/borg`
(mount a volume there), and an entrypoint that requires `BORG_HALLS` and
derives `OVERLAY_TOPICS` from it when unset.
`deploy/quickstart/compose.yaml` runs two of them. The reference host
requires `OVERLAY_ADMIN_TOKEN` (the bearer token of its `/admin` routes);
when it is unset the entrypoint sets a random one for that run and logs
so, which leaves `/admin` unusable until you set your own.

The host must run with no broadcaster, and must not enable an unordered
synchronization of unspent outputs for `tm_borg_` topics (spec sections
11.1 and 11.6). The reference host has neither unless configured.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `BORG_HALLS` | required | the hall identifiers this host carries, a comma list. Every `tm_borg_` topic in `OVERLAY_TOPICS` must be one of them, or the module refuses to start: a borg topic left to the host's default manager would admit anything |
| `BORG_STATE_DIR` | required | an absolute directory kept for the host's life (below) |
| `BORG_HEADERS_URL` | `OVERLAY_CHAIN_TRACKER_URL` when it is an `http` or `https` URL | the header source admission checks proofs against, in the reference host's native shape (`/v1/root/<height>`, `/v1/tip`). Required when the host's own source is another kind |
| `BORG_RETENTION_DAYS` | 31 | how long a host keeps what it no longer answers (below). At least 31 |
| `BORG_MAX_BEEF` | 262144 | the host's BEEF bound, in bytes. At least 262144 |
| `BORG_MAX_SWEEP_INPUTS` | 1024 | the most inputs of a sweep the host accepts. At least 1024 |
| `BORG_SPEND_TABLE` | 65536 | the spends of outpoints no carrier used that the host keeps (below). At least 65536 |
| `BORG_LISTEN` | none | `host:port` of the terms route (below) |
| `BORG_PRICES` | none | the classes this host prices, for example `history=5,history-after=5`. Only the priceable classes; a price of 0 charges nothing. A class and its `-after` form are priced together or not at all |
| `BORG_PAYEE_KEY` | none | the payee's private key, 64 lowercase hex characters: the terms route's BRC-104 identity and the key payments are derived from |
| `BORG_SESSIONS` | 10000 | the most BRC-104 sessions the terms route keeps. Past it the least recently used is forgotten |
| `BORG_SESSION_TTL` | 600 | seconds a BRC-104 session is kept once idle |
| `BORG_HANDSHAKES_PER_SEC` | 4 | BRC-104 handshakes a second the terms route answers; decimals allowed |
| `BORG_HANDSHAKE_BURST` | 8 | the most handshakes the terms route answers at once |
| `BORG_HANDSHAKES_PER_ADDR_PER_SEC` | 1 | handshakes a second one remote address is answered; decimals allowed |
| `BORG_HANDSHAKE_ADDR_BURST` | 4 | the most handshakes one remote address is answered at once |
| `BORG_RESPONSES_PER_SEC` | 5 | signed responses a second one BRC-104 session is given; decimals allowed |
| `BORG_RESPONSE_BURST` | 20 | the most signed responses one session is given at once |
| `BORG_ARCADE_URL` | none | arcade, which the host broadcasts each payment through before it answers (`POST /tx`, `GET /tx/<txid>`) |
| `BORG_ASSET_URL` | none | the node's asset service: the spend view of a payment's inputs and the proof of a held or watched payment. Without both URLs every payment is held until it mines |
| `BORG_ACCEPT_THRESHOLD_SATS` | 25000000 | the largest payment answered on the network's acceptance; above it a payment is held until it mines |
| `BORG_ACCEPT_PAYER_LIMIT` | the threshold | satoshis one payer may have answered fast and not yet mined; past it, held |
| `BORG_ACCEPT_TOTAL_LIMIT` | ten thresholds | the same across every payer |
| `BORG_ACCEPT_WINDOW` | `1h` | how long a fast payment counts against the limits unless it mines first (`ms`, `s`, `m`, `h`) |
| `BORG_ACCEPT_WATCH` | `0s` | how long the host keeps watching for a conflict after arcade's acceptance before it answers |

The host spells its header source `BORG_HEADERS_URL`; the `borg` command
spells its own `BORG_HEADER_URL` (the `header_url` key,
[configuration.md](configuration.md#keys)).

The generic terms route, budget, acceptance and state directory variables
(`BORG_SESSIONS` through `BORG_ACCEPT_WATCH`) behave as
[the host module](https://github.com/lightwebinc/bcommon/blob/main/docs/host-module.md) describes for `<APP>_`.

A price needs `BORG_LISTEN` and `BORG_PAYEE_KEY`. `history` and
`history-after` sell one walk, so a host that prices one and not the other
(`history=5` alone) is refused: the free form would answer every page but
the first for nothing. Any mistake stops the host before its port opens.

### The header source

The engine checks a token's own proof before the topic manager runs, but
not its parent's, and the reference host hands a module no chain tracker.
So the module reads headers itself, from the same kind of source the host
reads, and reuses a confirmed root for ten minutes. A source that does not
answer refuses a token or sweep `unmined`; the refusal is raised, so the
engine records nothing and the same object is decided again when it is
offered again.

## Admission

Every refusal is raised (spec section 11.1), so the engine stores nothing
for it and a later submission of the same txid is decided again: a copy
offered first in a worse BEEF cannot suppress the genuine object, and a
token refused while the host's headers lagged is admitted once they catch
up. Admission never reads another topic or anything the topic holds.

## The index

`ls_borg` keeps its index in memory and rebuilds it on start from the
engine's storage and the state directory (below), whatever order storage
returns rows in. Tokens the next token spent are not among the unspent rows
the host hands a module; the module reads them back from the rows it keeps,
and failing that walks back from each held token through the input that
spends its predecessor, reading each as the engine stores it (the token
alone with its proof). An admission that arrives while the index is being
rebuilt waits for the rebuild, and is then decided as any other.

At most 8 tokens are kept at one `seq` of a chain: those that are not
`pending` first, then the lowest txids. The set is a function of what the
host holds, never of arrival order, and a token on a predecessor no host
has cannot take the place of one on the chain. A `pending` token turned
away from a full `seq` stays in the engine's storage; once the predecessor
it names is decided the module reads it back and it takes its place.

### Statuses

A carrier is `retracted`, `superseded` or `held`, and a token `pending`,
`consistent`, `roll-mismatch`, `policy-mismatch`, `members-mismatch`,
`commitment-reuse` or `rejoin-unrotated` (spec section 11.2). A status never refuses anything; it
decides what is answered:

- a retracted or superseded carrier is not answered, except that `roll` and
  `member` answer every grant the roll holds, and `policy`, `role` and
  `bound` every policy record the policy set holds, whatever its status, so
  that a reader computing a root sees each and checks it;
- `roll`, `member`, `member-of`, `policy`, `role` and `bound` use the fold
  of a chain's highest token
  that is not `pending`, the first mined among several, so a token that
  arrived before its predecessor never hides the last complete state;
- `org`, `group` and their `-seq` classes answer every token held at the
  `seq` asked, whatever its status, and a reader decides.

No answer carries a token's status: a reader folds the chain itself
(spec section 13). The host reports what it found in its metrics and its
log: `borg_token_status_total{status}` counts each token's status once it is
decided, `borg_grants` and `borg_policies` are the carriers of each kind the
host holds, `borg_tokens_pending` is the tokens still waiting for a
predecessor, and every status other than `consistent` is logged with its
hall, organization, chain, `seq` and txid.

### The spend table

A sweep's spend of an outpoint some carrier spent joins that outpoint's row
and is kept for the host's life. A spend of any other outpoint waits in a
table of at most `BORG_SPEND_TABLE` outpoints, oldest dropped first: a
carrier that arrives later on a waiting outpoint is retracted at once and
the spend joins its row. A sweep whose every spend has left the table is
dropped and deleted from storage, so a chain of mined sweeps of a
stranger's own outputs costs the host the table and nothing more. The
table is rebuilt on start from the sweeps storage holds. A held sweep whose
tombstone an admitted transaction then spends is no longer among the
unspent rows storage hands a restart, so the state directory names it and
the restart reads it back: a spend that waits in the table still retracts
a carrier that arrives after a restart.

One mined sweep may be published in several halls. It is indexed in each
hall that admitted it, and dropping it from one topic leaves it answering
in the others.

### Retention

Kept for the host's life: tokens, outpoint rows and the sweeps they record.
A grant is kept while the roll the host answers holds it; one that left
the roll, or was swept, is kept `BORG_RETENTION_DAYS` after the later of
the two, and one never admitted to a roll is kept until it is swept. The
wraps of a group's current epoch are kept; those of an earlier epoch for
`BORG_RETENTION_DAYS` after the host first saw the token that ended it.
Up to 8 superseded wraps and 64 superseded grants are kept on one outpoint
as evidence, and the rest are dropped at once. Retention runs hourly.

What the index drops is deleted from the engine's storage at once: after
the admission that dropped it, and after each retention run. A storage
with no `deleteOutput` keeps what was dropped; the module says so in the
log once and keeps no list of it.

## The state directory

- `borg-rows.jsonl` holds what the host keeps for its life beside the
  engine's storage: every carrier any hall admitted with the funding output
  it spent, every drop, every spend of such an outpoint by an admitted
  sweep, when the host first saw each token, and each held sweep whose
  tombstone was spent. It keeps a funding output from answering a second
  carrier and keeps retention's clocks, so it is kept for the host's life,
  backed up with the database, and never edited. Each line is flushed to
  disk before the admission it records completes.
- `payments.jsonl` and `unsettleable.jsonl` are the generic payee ledger
  and watch files of [the host module](https://github.com/lightwebinc/bcommon/blob/main/docs/host-module.md#the-state-directory),
  which also covers settling, the backup script and restoring.

`borg-rows.jsonl` is append-only like them, so a copy taken while the host
runs is safe. Restore it only when it is lost or damaged, never to roll the
host back: rows written after the copy are gone.

The module holds every carrier line of `borg-rows.jsonl` in memory for the
host's life, and nothing bounds it: about 1 KB of memory and 300 bytes of
journal for each carrier ever admitted, dropped or not, so 1,000,000
carriers cost about 1 GB of memory and 300 MB of disk
(`borg_carrier_lines` counts them). Tokens cost a line each as well. Give
Node the heap for the carriers the host expects (`--max-old-space-size`),
and watch the gauge.

A state directory written by an earlier build loads unchanged: the lines it
holds are the lines this build writes, and a `payments.jsonl` line without
its outpoints has them read from its transaction. The reverse does not
hold once a spent sweep has been named: an earlier build refuses a journal
with a line it does not know (`"k":"w"`) and does not start. To run an
earlier build again, remove those lines from a copy of `borg-rows.jsonl`
first.

## The terms route and payment acceptance

With `BORG_LISTEN` set the module serves the terms route, its session,
handshake and response budgets, replay refusal and payment acceptance
exactly as [the host module](https://github.com/lightwebinc/bcommon/blob/main/docs/host-module.md) describes, with `<app>` = `borg`.
What is borg's own:

- the terms document is spec section 10.3, at `GET /ls_borg/terms`, and
  the priceable classes are `history` and `history-after`;
- `POST /lookup` answers the same BRC-24 questions as the host's
  `/lookup`, a `member` answer's context included;
- a payer's next payment may spend the change of its last, which is
  another coin;
- a held payment is answered 202 `ERR_PAYMENT_HELD`, and a response over
  the session budget makes `borg history` wait as `Retry-After` says (1 to
  30 seconds) and ask once more;
- `borg payee settle` reads the ledger's `decision` and `reason`.

## Metrics

| Metric | What |
| --- | --- |
| `borg_admitted_total{kind}`, `borg_admitted_repeats_total{kind}` | transactions admitted (`grant`, `policy`, `wrap`, `org-token`, `group-token`, `sweep`), each once; a txid the engine showed the manager again |
| `borg_refused_total{reason}` | refusals, by the reason labels of spec sections 3.1 and 11.1 |
| `borg_token_status_total{status}` | tokens by the status the fold gave them |
| `borg_retractions_total` | spends of a carrier's funding outpoint |
| `borg_dropped_total{why}` | carriers dropped as `evidence` or by `retention`, and sweeps dropped from the `table` |
| `borg_lookups_total{class}` | questions answered |
|  `borg_halls`, `borg_tokens`, `borg_tokens_pending`, `borg_grants`, `borg_policies`, `borg_wraps`, `borg_sweeps`, `borg_outpoint_rows`, `borg_spend_table` | what the index holds |
| `borg_carrier_lines` | carriers ever admitted, each a journal line held in memory for the host's life |
| `borg_sessions`, `borg_sessions_evicted_total{why}`, `borg_handshakes_total{result}` | the terms route |
| `borg_payments_total{decision,reason}` | paid questions by what the host did: `requested` (`no-payment`, a 402 with the price); `fast` (`at-or-below-threshold`, `mined`); `hold` (`above-threshold`, `payer-limit`, `total-limit`, `payer-flagged`, `no-broadcast-leg`, `broadcast-unconfirmed`, `no-network-verdict`, `double-spend-attempted`, `spend-view-unknown`); `refuse` (`malformed`, `wrong-script`, `spv-failed`, `not-final`, `outputs-exceed-inputs`, `network-refused`, `double-spent`, `replayed`, `conflict`) |
| `borg_payment_events_total{kind}` | fast payments the watch resolved: `confirmed`, `double-spent`, `refused`, `unmined` (all but the first flag the payer) |
| `borg_payments_unmined` | fast payments being watched |
| `borg_requests_total{result}` | authenticated requests refused before any signature: `replayed`, or `limited` by the session's budget |

## Testing against a reference host

`make e2e REFERENCE_HOST=/path/to/reference-host` runs the bundle on a
local reference host (its compiled `dist/index.js` and its `node_modules`)
with a throwaway MySQL in Docker and a local chain: it creates an
organization, grants three members, puts them in a group, opens the epoch
key as each from the host's answers, removes one (who cannot open the next
epoch), rotates, revokes by the roll and by a sweep, replays a suppression
attempt, pays a priced question's 402, restarts the host and asks
everything again of the restored index.
