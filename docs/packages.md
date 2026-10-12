# Go packages

borg is a command and a host module. Two of its Go packages are also public,
so that another application can read borg's chains by borg's own code instead
of a copy of it. Everything else stays under `internal/` and cannot be
imported.

| Package | What it is |
| --- | --- |
| `github.com/lightwebinc/borg/borgrec` | The byte-level contract ([spec.md](spec.md)): records, tokens, wraps, the folds, the registry values and the bounds. No network. |
| `github.com/lightwebinc/borg/reader` | A client that asks overlay hosts the `ls_borg` questions and verifies every answer against the caller's own headers. |

## Supported surface

These are what an importer may rely on. The packages export more than this,
because borg's own command uses them; anything not listed here may change at
any tag.

**`borgrec`**

- Group records: `Group`, `DecodeGroup`, `Group.BeginsEpoch`,
  `CheckGroupTransition`, `Fold`, `RejoinUnrotated`, `MembersRoot`,
  `GroupFold`, and the `Status*` values.
- A group's chain from its records alone: `Link`, `FoldGroup`, `GroupChain`
  and `Epoch` (each epoch's id, commitment, seq, txid and height). It asks
  no host and verifies no proof.
- Epoch keys: `EpochID`, `WrapRecord`, `DecodeWrap`, `Entry`, `CheckEntry`,
  `ParseEntry`, `CheckEpochToken`, `Unwrap`, `ReleaseKeyID`.
- Tokens: `ReadToken`, `StoredToken`, `ChainToken`.
- The signer key (spec section 7.4): `SignerKey`, which anyone derives
  from the organization's public key, and `SignerPrivate`, which only the
  organization's identity key derives. An application run from the
  organization's home opens the group's epoch keys with it, through the
  wraps the organization makes to its own signer key.
- Policy records (spec section 5A): `Policy`, `DecodePolicy`,
  `Policy.IsBinding`, `Policy.BoundToGroup`, `PolicyRole`, `PolicyBinding`,
  `PolicyRoot`, and on the organisation record the policy keys and
  `Successor`.
- The permission grammar: `Permission`, `ParsePermission`, `Asked`,
  `Permission.Grants`, `GrantsAny`, `Intersect` and `CheckRoleName`.
- Effective permissions (spec section 5A.4): `Subject`, `Effective` and
  `Allows`, which fold records the caller has already verified. They reach
  no host and check no proof; what a service must establish first is the
  `Subject` doc comment.
- The registry values (protocol name, key ids, tags, magics, derivations,
  topic prefix and lookup service), `CheckHall`, the bounds (`Max*`, `Min*`,
  `*Len`), the `Err*` values and `Reason`.

**`reader`**

- `Client`, with `Client.Chain`, `Client.Group`, `Client.Open`,
  `Client.Wraps`, `Client.Recipients`, `Client.GroupsAll` and `Client.Ask`
  (one question to one host, the answer bounded by `Client.MaxAnswer`).
- `Client.Token`, `Client.Carrier` and `Client.Sweep`: the three checks
  below against the client's headers, with a proof a host stored that no
  longer verifies replaced by the transaction's current proof from
  `Client.Source` (a `ChainSource`; bcommon's `nodeapi.Asset` is one).
- `ChainView` (with `Beyond`: the verified tokens above the chain read),
  `GroupView`, `NewGroupView`.
- The policy set: `Client.PolicySet` (every host, all pages, each answer
  held to the root the latest token commits to), `Client.RoleOf`,
  `Client.BoundTo`, `OrgView.PolicyPathHolds`.
- The decision a service makes: `Client.Effective` and `Client.Allows`, the
  procedure of spec section 13 for one subject, and `Decision`, which
  carries the verdict, the permissions held, and the token, grants, policy
  records and groups it rested on. Both fail closed: a read that could not
  be completed is `incomplete` and denies.
- `Latest`, `Fresh`, `Spends`. `Latest` takes unspent only on a spend
  source's `""` with a nil error; with bcommon's `nodeapi.Asset` that is
  the node's status `OK`, and every other answer is an error wrapping
  `nodeapi.ErrSpendUnknown`, which is no evidence.
- `QuietSpend`: a spend source's error with the names the node chose left
  out of its text (a short status word kept), for printing; `errors.Is`
  still holds.
- `VerifyToken`, `VerifyCarrier`, `VerifySweep`.
- `Floor` and `RollbackError`: see below.
- `NoChainError`: a chain whose answered tokens none verified, with each
  refusal naming its host. `errors.Is(err, ErrNoChain)` holds for it, and
  a caller tells it apart from an empty answer with `errors.As`.
- `SubjectID`, `ParseGroup`, `GroupHex`.
- What those return: `Tok`, `Car`, `Opened`, `Refusal`.

`Client.Open` asks every host for the member's wrap and checks every wrap
each answers, after one has opened the key. A wrap a host answered that
does not verify, or that is not of the epoch asked, is in
`Opened.Refused`, naming the host, whether or not the key opened; a host
that could not be asked is in `Opened.HostErr`. A caller names each and
treats the read as incomplete. When the key did not open, `Opened.Label`
says why, as before: `entry` (no wrap to this key that verifies) and
`token` (only wraps of another epoch) are absences when the caller's key
was a member of the epoch by the group's chain (`GroupView.EpochsOf`).

An answer to one unpaged question that holds more outputs than a page
(128), or more than 16 tokens to one seq, is a host fault: only the bound
is read, and a `Refusal` names the host (`ErrOverfull`), in
`Opened.Refused`, the refusals of `GrantsOf` and `ChainView.Refused`. A
paged walk cut at the reader's bound is a `Refusal` of `Wraps` and an
error of `GroupsOf` and `GroupsAll` (`ErrTruncated`). The same BEEF bytes
answered again in one call are not verified again.


### What `reader` does not do

`reader` keeps no state between calls: it reads no file, and remembers
nothing it verified. Every token it returns is verified against the
caller's headers and linked from seq 0, so what it returns is a true
chain; it may not be the whole chain. Hosts that all withhold the latest
tokens answer an older state, every token of which verifies.

- `ChainView.Beyond` reports verified tokens the hosts answered above the
  end of the chain read. That catches a token withheld in the middle. It
  does not catch hosts that withhold everything from some seq on.
- Only the caller's own memory catches that. A caller that has somewhere
  to keep it sets `Client.Floor`: `Chain` then asks it for the seq and
  txid of the latest token the caller verified of the chain, refuses with
  a `*RollbackError` a chain that ends below that seq, sets
  `ChainView.Replaced` when the token at that seq is another one (a
  reorganization, or a fork: the caller decides), and hands it the latest
  token read. The implementation must persist what it is handed before the
  caller acts on the chain, and must not lower a seq on its own.
- A caller with no `Floor` has no rollback memory for a group's chain. It
  should name more than one host, treat `Beyond` as incomplete, and where
  it keys new content on the chain's latest token, hold that token to a
  spend source first (`Latest`).

`Chain` walks seq by seq and stops at the first seq nothing links to, so
its work is bounded by the chain and by `Client.MaxPages`, never by a seq
a host names. An answer is at most `Client.MaxAnswer` bytes (16 MiB by
default), and at most 16 tokens are taken from one answer to a seq.

## Versions

The packages are versioned by the repository's tags, as plain semantic
versions (`v0.1.0`). An importer pins a tag in its `go.mod`; there is no
other release channel, and an untagged commit carries no promise. While the
major version is 0, a minor version may change the supported surface. The
bytes the packages read and write are the
contract of [spec.md](spec.md) and [frozen.md](frozen.md), held by the
golden vectors under `testdata/vectors`; a tag never changes those.

borg pins go-sdk at exactly v1.7.1 and bcommon at the version in its
`go.mod`. Go's minimal version selection gives an importer the higher of
its own pin and borg's, so an importer that needs the same bytes pins the
same versions and asserts them, as borg's `make deps-check` does.

## Importing a private module

The repository is private. The public Go proxy and checksum database cannot
see it, so the `go` command has to be told to go to the repository directly,
and needs credentials for it:

```console
$ go env -w GOPRIVATE='github.com/lightwebinc/*'
$ git config --global url."ssh://git@github.com/lightwebinc/".insteadOf "https://github.com/lightwebinc/"
$ go get github.com/lightwebinc/borg@v0.1.1
```

`GOPRIVATE` turns off the proxy and the checksum database for the matching
paths. The `insteadOf` line makes git fetch over SSH with the key the
machine already uses; an HTTPS credential helper or a token in `~/.netrc`
with read access to the repository does the same. A build without
credentials (a CI job with none, for instance) can instead resolve the
module from a copy of its tagged archive served as a file proxy
(`GOPROXY=file://...`), which `go.sum` still verifies against the tag.
