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
| `BORG_PRICES` | none | the classes this host prices, for example `history=5,history-after=5`. Only the priceable classes; a price of 0 charges nothing |
| `BORG_PAYEE_KEY` | none | the payee's private key, 64 lowercase hex characters: the terms route's BRC-104 identity and the key payments are derived from |
| `BORG_SESSIONS` | 10000 | the most BRC-104 sessions the terms route keeps. Past it the least recently used is forgotten |
| `BORG_SESSION_TTL` | 600 | seconds a BRC-104 session is kept once idle |
| `BORG_HANDSHAKES_PER_SEC` | 4 | BRC-104 handshakes a second the terms route answers; decimals allowed |
| `BORG_HANDSHAKE_BURST` | 8 | the most handshakes the terms route answers at once |
| `BORG_HANDSHAKES_PER_ADDR_PER_SEC` | 1 | handshakes a second one remote address is answered; decimals allowed |
| `BORG_HANDSHAKE_ADDR_BURST` | 4 | the most handshakes one remote address is answered at once |

A price needs `BORG_LISTEN` and `BORG_PAYEE_KEY`. Any mistake stops the
host before its port opens.

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
alone with its proof).

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
table is rebuilt on start from the sweeps storage holds.

### Retention

Kept for the host's life: tokens, outpoint rows and the sweeps they record.
A grant is kept while the roll the host answers holds it; one that left
the roll, or was swept, is kept `BORG_RETENTION_DAYS` after the later of
the two, and one never admitted to a roll is kept until it is swept. The
wraps of a group's current epoch are kept; those of an earlier epoch for
`BORG_RETENTION_DAYS` after the host first saw the token that ended it.
Up to 8 superseded wraps and 64 superseded grants are kept on one outpoint
as evidence, and the rest are dropped at once. Retention runs hourly.

## The state directory

- `borg-rows.jsonl` holds what the host keeps for its life beside the
  engine's storage: every carrier any hall admitted with the funding output
  it spent, every drop, every spend of such an outpoint by an admitted
  sweep, and when the host first saw each token. It keeps a funding output
  from answering a second carrier and keeps retention's clocks, so it is
  kept for the host's life, backed up with the database, and never edited.
  Each line is flushed to disk before the admission it records completes.
- `payments.jsonl` holds every payment the terms route accepted: the Atomic
  BEEF, the derivation prefix and suffix, the sender's identity key, the
  satoshis and the class. The route answers once the line is on disk. It
  does not broadcast the payment.

Both files are append-only, so a copy taken while the host runs is safe: at
worst its last line is cut short, and both readers skip such a line. Copy
the directory daily beside the database backup and keep several days.
Restore it only when it is lost or damaged, never to roll the host back:
rows written after the copy are gone.

A payment is money only once it is broadcast and mined; until then the
payer can spend the same coins elsewhere. Settle each line promptly with
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
asker, the prefix is one this route issued, the payment verifies against
the host's headers, and its txid was never used before. One payment buys
one question. The same class asked on the host's own `/lookup` is refused
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
  route's, or limit at the proxy.

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
| `borg_sessions`, `borg_sessions_evicted_total{why}`, `borg_handshakes_total{result}`, `borg_payments_total{result}` | the terms route |

## Testing against a reference host

`make e2e REFERENCE_HOST=/path/to/reference-host` runs the bundle on a
local reference host (its compiled `dist/index.js` and its `node_modules`)
with a throwaway MySQL in Docker and a local chain: it creates an
organisation, grants three members, puts them in a group, opens the epoch
key as each from the host's answers, removes one (who cannot open the next
epoch), rotates, revokes by the roll and by a sweep, replays a suppression
attempt, pays a priced question's 402, restarts the host and asks
everything again of the restored index.
