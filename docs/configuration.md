# Configuration

Every setting of the `borg` command, from the code
(`internal/config` and `cmd/borg`). The overlay host's environment
(`BORG_HALLS`, `BORG_HEADERS_URL`, prices, the payee key and the rest) is
in [host.md](host.md#configuration).

## Where settings come from

Highest first:

1. global flags, before the command (`borg -network test roll`);
2. `BORG_<KEY>` in the environment, the key in upper case
   (`BORG_HEADER_URL=woc:main`);
3. the config file;
4. the built-in defaults below.

The config file is `-config PATH`, else `$BORG_HOME/config`, else
`$XDG_CONFIG_HOME/borg/config`, else `~/.borg/config`. Its grammar is one
`key = value` per line, `#` comments, blank lines ignored. A key it does
not define, a key given twice, or a line without `=` is an error, never
ignored: a misspelled key would otherwise leave a setting at its default
with no sign.

The addresses (`hosts`, `facade`, `header_url`, `asset`, `settle`, `rpc`,
`history_host`) have no default on purpose: a default would send an
organization's records, or a member's questions, to a server nobody
configured. A command that needs one and finds none exits 2 naming the key.

## Keys

| Key | Default | Meaning |
| --- | --- | --- |
| `arcade_key` | none | bearer token for an arcade installation named in `settle` |
| `asset` | none | a Teranode node's asset HTTP API base URL (`/api/v1/...`): proofs, the chain tip, raw transactions, and the node's view of which outputs are spent (the spend source of revocation checks and writer evidence) |
| `chain_tree_count` | `8` | outputs of a new chain tree, 1 to 32: a chain's first token carries its chain tree whole |
| `delay` | `60s` | the propagation delay: a mined token or sweep is published this long after its block is in this home's headers, so the hosts' headers have it too (spec section 7.2); `0s` to `10m` |
| `entries` | `8` in mode `plane`, `16` in `unicast` | the most wrap entries in one wrap record, 1 to 16 |
| `facade` | none | mode `plane`: the overlay host a publisher submits to, from which the plane delivers to every subscribed host. Refused in mode `unicast` |
| `freshness` | `60s` | how old a writer's evidence that a group's latest token is the latest may be (spec section 6.5); at most `600s` |
| `hall` | none | the hall identifier a command uses; a home that runs an organization uses its own |
| `header_url` | none | the header source every proof is checked against ([header sources](#header-sources)): `woc:main`, `woc:test`, `chaintracks:URL`, or an overlay bridge's base URL |
| `history_host` | none | the base URL of `ls_borg` on the host `history` asks and `terms` reads: the host's terms route (spec section 10.3). An origin, with no path |
| `home` | `~/.borg` | the identity's home |
| `hosts` | none | the overlay hosts, comma separated, at most 16: every host a reader asks and compares; in mode `unicast` every host a publisher submits to; in mode `plane` the hosts every mined object is also sent to directly |
| `mode` | `plane` | `plane` or `unicast`: how a publisher's objects reach the hosts (spec section 12) |
| `network` | `main` | `main`, `test` or `regtest`: the fund address prefix and the header source's proof-of-work floor |
| `object_bound` | `1048576` | the plane's object bound in bytes, at most 8388608; a carrier whose BEEF is larger is not published |
| `org` | none | the organization a reader reads: its identity key |
| `originator` | `borg` | the BRC-100 originator presented to the wallet |
| `quorum` | `all` | how many of `hosts` must hold a carrier (a grant or a wrap) for it to count as published: `all`, `majority`, `one`, or a number up to the host count. In unicast a host that took it counts; on the plane a host that answers it by lookup does. A token or a sweep always needs every host |
| `rpc` | none | the node's JSON-RPC URL: `fund` without `-txid` (coinbase: only on a regtest chain you run, for development and tests) and `settle = rpc:` |
| `rpc_pass`, `rpc_user` | `bitcoin` | the node's basic auth, also for `settle = rpc:` |
| `settle` | none | the settlement leg for what is mined (funding trees, tokens, sweeps, a payment a payee takes): `arcade:<url>` (any ARC-compatible API, with `arcade_key`), `rpc:<url>` (a node's JSON-RPC `sendrawtransaction`, with `rpc_user` and `rpc_pass`) or `tcp:<host:port>` (raw transactions to an ingress, no acknowledgement) |
| `signers` | `1` | how many homes of the organization can sign transitions, 1 to 16. Above one: wraps wait one further block after their token, the signer key is kept a member of every group, and each command first catches up with what another home did (spec sections 7.3 and 7.4) |
| `timeout` | `15s` | bounds each request; `1s` to `5m` |
| `tree_count` | `32` | outputs of a new grant or record tree, 1 to 1000 |

`hall` must be a hall identifier `<name>_<suffix>`; `org` 66 lowercase hex
digits; `mode`, `network` and `quorum` only the values listed; the counts
and durations their ranges. Anything else exits 2 naming the key and the
limit ([limits.md](limits.md)).

## What each role needs

| Role | Keys |
| --- | --- |
| A member or any reader (`roll`, `member`, `member-of`, `group show`, `open`) | `hosts`, `header_url`, `hall`, `org`; `asset` for revocation checks, writer evidence (`open -write`) and replacing a proof a reorganization left stale |
| The organization (every command that publishes) | the reader's, and `asset`, `settle`, and `facade` in mode `plane` |
| Funding a home on mainnet or testnet (`fund -txid`) | `asset`, `header_url` |
| The priced question (`history`) | the reader's, `history_host`, `asset`, `settle`, and coin in the home |
| A host's payee (`payee settle`) | `header_url`, `asset`, `settle` |
| A local regtest chain only (`fund` without `-txid`) | `rpc`, `asset`; coinbase: only on a regtest chain you run (development and tests) |

## Networks

| `network` | Fund address | Header source | Coinbase funding |
| --- | --- | --- | --- |
| `main` (default) | starts with `1` | `woc:main`, or your own chaintracks or bridge | refused |
| `test` | starts with `m` or `n` | `woc:test`, or your own | not answered by a public testnet node; use `fund -txid` |
| `regtest` | starts with `m` or `n` | the regtest chain's own (the sandbox's `devchain`) | `fund` mines it (coinbase: only on a regtest chain you run, for development and tests) |

`network` sets the fund address prefix and the proof-of-work floor of a
header source. A `woc:` source must name the same network: `-network main
-header-url woc:test` exits 2 (`header source woc:test is the test network
but network is main`).

## Header sources

Every proof is checked against the block headers of one source, so the
source is the root of trust for every verified answer.

| `header_url` | Source |
| --- | --- |
| `woc:main`, `woc:test` | the public WhatsOnChain API: each header is hashed here and must carry the work its bits claim |
| `chaintracks:https://host/v2` | a chaintracks v2 service, public or your own, checked the same way |
| `https://host:port` | an overlay bridge's header read API (`GET /v1/tip`, `GET /v1/root/<height>`). It serves roots, not headers, so run it yourself |

```console
$ borg -network main -header-url woc:main doctor | grep headers
headers     woc:main tip 970055
```

## Global flags

| Flag | Key | Meaning |
| --- | --- | --- |
| `-config PATH` | none | the config file |
| `-home DIR` | `home` | the identity's home |
| `-hall HALL` | `hall` | the hall identifier |
| `-org KEY` | `org` | the organization a reader reads |
| `-hosts URLS` | `hosts` | overlay hosts, comma separated |
| `-header-url SOURCE` | `header_url` | the header source |
| `-mode MODE` | `mode` | `plane` or `unicast` |
| `-network NET` | `network` | `main`, `test` or `regtest` |
| `-quorum Q` | `quorum` | `all`, `majority`, `one` or N |
| `-timeout DUR` | `timeout` | per request |
| `-v` | none | verbose; also accepted after the command |
| `-version` | none | print the version and exit |

## Command flags

`borg <command> -h` prints them; every one is listed here.

| Command | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `init` | `-from FILE` | none | make the home around another home's `identity.json` |
| `fund` | `-txid TXID` | none | import this mined payment you sent to the fund address (mainnet, testnet) |
| `fund` | `-blocks N` | `101` | coinbase: only on a regtest chain you run (development and tests); blocks to mine, or with `-rescan` to re-read |
| `fund` | `-batch N` | `30` | coinbase: only on a regtest chain you run (development and tests); blocks per `generatetoaddress` call |
| `fund` | `-rescan` | off | coinbase: only on a regtest chain you run (development and tests); re-read recent blocks for coinbase the pool lacks |
| `org create` | `-member KEY` | none | a first member; repeatable |
| `org create` | `-opaque` | off | publish grants without their fields in the clear |
| `org create`, `org add`, `grant issue` | `-role`, `-scope`, `-expiry`, `-admin` | `member`, `member`, one year, off | the grant's role (1 to 64 bytes), scope, expiry (a duration such as `8760h` or `YYYY-MM-DDTHH:MM:SSZ`), and the `admin` scope mark |
| `org add`, `org remove` | `-group G` | none | the group |
| `org due` | `-within DUR` | `720h` | also list members whose grant expires within this long |
| `org due` | `-apply` | off | make the removals due now |
| `org sync` | `-retire-trees` | off | stop spending this home's current grant and record trees (a copied home) |
| `grant revoke` | `-by` | `both` | `both`, `roll` or `sweep` |
| `group create` | `-member KEY` | none | a member; repeatable |
| `group create` | `-rotate-on-join` | off | every addition begins a new epoch |
| `group create` | `-rotate-every DUR` | none | the rotation schedule intended (informative; `doctor` says when it is overdue) |
| `open` | `-write` | off | require writer evidence that the latest token is unspent |
| `open` | `-show-key` | off | print the current epoch key (a secret) |
| `history` | `-at URL` | `history_host` | the host's terms route |
| `history` | `-chain` | `org` | `org`, or a group id |
| `history` | `-after SEQ` | none | the page after this seq |
| `history` | `-all` | off | every page from there on, each a paid question |
| `history` | `-max-sats N` | `1000` | the most one question is paid |
| `history` | `-budget N` | `16000` | the most the command pays in all |
| `payee key` | `-out FILE` | none | the file to write `BORG_PAYEE_KEY` to (`-` for standard output) |
| `payee settle` | `-in-flight N` | `16` | the most payments broadcast and not yet mined at once |
| `kill` | `-confirm KILL` | none | must be the literal `KILL` |
| `kill` | `-yes` | off | actually sweep |

## Environment variables

| Variable | Effect |
| --- | --- |
| `BORG_<KEY>` | one per key above, for example `BORG_HEADER_URL`, `BORG_SETTLE`. Overrides the file; a flag overrides it |
| `BORG_HOME` | the home **and** where the config file is looked for first |
| `XDG_CONFIG_HOME` | the second place the config file is looked for |
| `HOME` | the default home is `$HOME/.borg` |

## Example files

An organization on mainnet, publishing to two hosts in unicast:

```text
# ~/.borg/config
network      = main
hall         = example_qzxkvbmwtr
header_url   = woc:main
asset        = https://asset.example.com
settle       = arcade:https://arc.example.com
arcade_key   = <your ARC token, if the service needs one>
mode         = unicast
hosts        = https://host-a.example.com,https://host-b.example.com
```

On a multicast plane, `mode = plane` and `facade = https://host-a.example.com`
(the host the plane takes submissions at).

A member on testnet:

```text
network      = test
hall         = example_qzxkvbmwtr
org          = 02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5
hosts        = https://host-a.example.com,https://host-b.example.com
header_url   = woc:test
asset        = https://asset-test.example.com
history_host = https://terms.host-a.example.com
```

## The container image

`ghcr.io/lightwebinc/borg` (built locally by `make docker-build`) is the
static binary on a distroless nonroot base, entrypoint `borg`. The home is
`/home/nonroot/.borg`: mount a named volume there, and pass settings as
`BORG_<KEY>` variables. The image bakes in no address. Licenses are in
`/usr/share/doc/borg/`.

```console
$ docker run --rm -v borg-home:/home/nonroot/.borg \
    -e BORG_NETWORK=main -e BORG_HEADER_URL=woc:main \
    ghcr.io/lightwebinc/borg doctor
```

A bind mount keeps the host directory's owner: make it writable by uid
65532, or run with `--user`. `ghcr.io/lightwebinc/borg-devchain` (the
`devchain` build target) is the sandbox's local regtest chain, and
`ghcr.io/lightwebinc/borg-host` the overlay host with the module
([host.md](host.md)).
