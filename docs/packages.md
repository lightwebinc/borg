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
- The registry values (protocol name, key ids, tags, magics, derivations,
  topic prefix and lookup service), `CheckHall`, the bounds (`Max*`, `Min*`,
  `*Len`), the `Err*` values and `Reason`.

**`reader`**

- `Client`, with `Client.Chain`, `Client.Group`, `Client.Open` and
  `Client.Wraps`.
- `ChainView`, `GroupView`, `NewGroupView`.
- `Latest`, `Fresh`, `Spends`.
- `VerifyToken`, `VerifyCarrier`.
- What those return: `Tok`, `Car`, `Opened`, `Refusal`.

## Versions

The packages are versioned by the repository's tags, as plain semantic
versions (`v0.1.0`). An importer pins a tag in its `go.mod`; there is no
other release channel, and an untagged commit carries no promise. While the
major version is 0, a minor version may change the supported surface. The
bytes the packages read and write are the
contract of [spec.md](spec.md) and [frozen.md](frozen.md), held by the
golden vectors under `testdata/vectors`; a tag never changes those.

borg pins go-sdk at exactly v1.5.2 and bcommon at the version in its
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
$ go get github.com/lightwebinc/borg@v0.1.0
```

`GOPRIVATE` turns off the proxy and the checksum database for the matching
paths. The `insteadOf` line makes git fetch over SSH with the key the
machine already uses; an HTTPS credential helper or a token in `~/.netrc`
with read access to the repository does the same. A build without
credentials (a CI job with none, for instance) can instead resolve the
module from a copy of its tagged archive served as a file proxy
(`GOPROXY=file://...`), which `go.sum` still verifies against the tag.
