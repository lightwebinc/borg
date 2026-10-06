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
`deploy/quickstart/compose.yaml` runs two of them.

The host must run with no broadcaster, and must not enable an unordered
synchronisation of unspent outputs for `tm_borg_` topics (spec sections
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
`consistent`, `roll-mismatch`, `members-mismatch`, `commitment-reuse` or
`rejoin-unrotated` (spec section 11.2). A status never refuses anything; it
decides what is answered:

- a retracted or superseded carrier is not answered, except that `roll` and
  `member` answer every grant the roll holds, whatever its status;
- `roll`, `member` and `member-of` use the fold of a chain's highest token
  that is not `pending`, the first mined among several, so a token that
  arrived before its predecessor never hides the last complete state;
- `org`, `group` and their `-seq` classes answer every token held at the
  `seq` asked, whatever its status, and a reader decides.

No answer carries a token's status: a reader folds the chain itself
(spec section 13). The host reports what it found in its metrics and its
log: `borg_token_status_total{status}` counts each token's status once it is
decided, `borg_tokens_pending` is the tokens still waiting for a
predecessor, and every status other than `consistent` is logged with its
hall, organisation, chain, `seq` and txid.

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
- `payments.jsonl` holds every payment the terms route accepted: the Atomic
  BEEF, the derivation prefix and suffix, the sender's identity key, the
  satoshis, the class and the outpoints it spends. The route answers once
  the line is on disk. The host broadcasts the payment ([Payment acceptance](#payment-acceptance)); the payee still settles it.

Both files are append-only, so a copy taken while the host runs is safe: at
worst its last line is cut short, and both readers skip such a line. Copy
the directory daily beside the database backup and keep several days.
Restore it only when it is lost or damaged, never to roll the host back:
rows written after the copy are gone.

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

A payment is money only once it is mined; until then the payer can race a conflicting spend to a miner. Settle each line promptly with
the payee wallet's BRC-100 `internalizeAction` (protocol `wallet payment`,
output 0, the remittance as written), which broadcasts it.

## The terms route

The reference host lets a module add no route of its own, so with
`BORG_LISTEN` set the module serves its own, beside the host:

| Route | Answer |
| --- | --- |
| `GET /ls_borg/terms` | the terms document of spec section 10.3; 404 when the host prices nothing |
| `POST /.well-known/auth` | the BRC-104 handshake |
| `POST /lookup` | the same BRC-24 questions as the host's `/lookup`, a `member` answer's context included |

Its base URL is what the host publishes as `ls_borg`'s base (BRC-180
`metanet.overlays`), an origin with no path: a BRC-104 client shakes hands
at the origin's `/.well-known/auth`. A reverse proxy in front of it gives
it an origin of its own and passes the path unchanged, because a BRC-104
signature covers it.

A free question is answered with or without BRC-104. A question of a class
the host prices must be asked over BRC-104: without an `x-bsv-payment`
header it is answered 402 with `x-bsv-payment-version` (`1.0`),
`x-bsv-payment-satoshis-required` and `x-bsv-payment-derivation-prefix`;
with one, it is answered once output 0 of the payment pays at least the
price, P2PKH, to the key BRC-29 derives for the prefix, the suffix and the
asker, the prefix is one this route issued, the payment verifies against the host's headers, its txid was never used before, it spends no coin an accepted payment spent, and it passes [payment acceptance](#payment-acceptance). Two transactions spending one coin can both verify, and at most one can ever be mined: the second is refused 409 `ERR_PAYMENT_CONFLICT`, as a
used txid is refused 409 `ERR_PAYMENT_REPLAYED`. The coins of every
accepted payment are kept in `payments.jsonl` and refused again after a
restart. A payer's next payment may spend the change of its last, which is
another coin. One payment buys one question. The same class asked on the host's own `/lookup` is refused
with an error that names this route.

A handshake is unauthenticated and costs the host a signature, so the
route bounds both its memory and its work:

- **sessions:** at most `BORG_SESSIONS`, each forgotten once idle for
  `BORG_SESSION_TTL` seconds, the least recently used first past the cap.
  A client whose session was forgotten shakes hands again;
- **handshakes:** two token buckets checked before the request body is
  read and before any signature work, the route's
  (`BORG_HANDSHAKES_PER_SEC`, `BORG_HANDSHAKE_BURST`) and each remote
  address's (`BORG_HANDSHAKES_PER_ADDR_PER_SEC`,
  `BORG_HANDSHAKE_ADDR_BURST`; IPv6 by /64). The address's is checked
  first, so a flooding address spends its own budget. A handshake over
  either is answered 429 with `Retry-After`. Behind a reverse proxy every
  client shares the proxy's address: raise the per-address limit to the
  route's, or limit at the proxy;
- **signed responses:** every authenticated request is answered with a
  signature, so one handshake must not buy signatures without end. Each
  session has a token bucket (`BORG_RESPONSES_PER_SEC`,
  `BORG_RESPONSE_BURST`), taken only by a request that verified; a request
  over it is answered 429 with `Retry-After` and no signature. A BRC-104
  client reports an unsigned answer as a failed authentication: `borg
  history` waits as the header says (1 to 30 seconds) and asks once more;
- **replays:** an authenticated request sent again byte for byte is
  refused 401 `ERR_AUTH_REPLAYED` before any signature work. The route
  remembers the last 65536 requests it verified; the session budget bounds
  what an older replay can cost.


## Payment acceptance

A paid question is answered only after the payment is held to bcommon's
acceptance rule (package `acceptance`, the same rule for every layer):

1. the route's own checks (prefix, output 0 pays the derived key, SPV
   against the host's headers, a txid and coins never used before);
2. finality (lock time 0, or every input final) and conservation (every
   input carries its source, and the outputs do not exceed the inputs);
3. the value: at or below `BORG_ACCEPT_THRESHOLD_SATS`, and within the
   payer's and the total limits, it is fast; otherwise it is held;
4. the host broadcasts it through `BORG_ARCADE_URL`, unmined ancestors first,
   held or fast: this is how the payee collects;
5. on the fast path the host answers only once arcade reports the network
   took it (`SEEN_ON_NETWORK`, `ACCEPTED_BY_NETWORK`, ...), with no
   `DOUBLE_SPEND_ATTEMPTED` and no competing transaction, and the node shows
   every input unspent or spent by this payment.

A refusal (arcade `REJECTED`, a definitive HTTP refusal, an input spent by
another transaction, a payment that is not final or spends more than it
holds) is answered 400 `ERR_PAYMENT_REFUSED`. Anything else that is not
fast (above the threshold, a limit reached, a conflict reported, no
verdict within 10 s, a spend view that cannot say) is answered 402
`ERR_PAYMENT_HELD` with the reason and no BRC-105 headers, so a client
does not pay again. The held payment is recorded in `payments.jsonl` with
`"decision":"hold"`, keeps its coins, and the same payment sent again is
answered once the node serves its proof and that proof verifies against the
host's headers. A host with no arcade or no node holds every payment.

Every fast payment is watched on the host's background tick (every 30 s)
until it mines. One whose input another transaction spent, that arcade
refuses, or that has not mined within a day, is written to
`unsettleable.jsonl` in the state directory, logged, and counted; its payer
is then held for confirmation on every later payment until the host
restarts. Nothing is clawed back: the answer was given. A restart watches
the fast lines of the last window again.

The ledger line carries `decision` (`fast`, `hold` or `mined`) and
`reason`; `borg payee settle` reads it as before.

## Metrics

| Metric | What |
| --- | --- |
| `borg_admitted_total{kind}`, `borg_admitted_repeats_total{kind}` | transactions admitted (`grant`, `wrap`, `org-token`, `group-token`, `sweep`), each once; a txid the engine showed the manager again |
| `borg_refused_total{reason}` | refusals, by the reason labels of spec sections 3.1 and 11.1 |
| `borg_token_status_total{status}` | tokens by the status the fold gave them |
| `borg_retractions_total` | spends of a carrier's funding outpoint |
| `borg_dropped_total{why}` | carriers dropped as `evidence` or by `retention`, and sweeps dropped from the `table` |
| `borg_lookups_total{class}` | questions answered |
| `borg_halls`, `borg_tokens`, `borg_tokens_pending`, `borg_grants`, `borg_wraps`, `borg_sweeps`, `borg_outpoint_rows`, `borg_spend_table` | what the index holds |
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
organisation, grants three members, puts them in a group, opens the epoch
key as each from the host's answers, removes one (who cannot open the next
epoch), rotates, revokes by the roll and by a sweep, replays a suppression
attempt, pays a priced question's 402, restarts the host and asks
everything again of the restored index.
