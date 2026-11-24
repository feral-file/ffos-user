# web3:// works on the FF1

Status: shipped in feral-controld (feral-file#3599). Reader package:
`components/feral-controld/web3`.

## Why

A generative work does not have to live in a file. Han Saglam's *Digital Soul* is the
return value of `artpiece()` on `0x4f32740cf412d647a622ba27c36fa4b76f71601f`, and Art
Blocks' generator is `getTokenHtml(address,uint256)` on
`0x953d288708bb771f969fcfd9ba0819ef506ac718`. ERC-4804 (with ERC-6860's corrections) gives
both an address a player can resolve with one `eth_call`:

    web3://<contract>[:<chainid>][/<method>[/<arg>…]][?<query>][#<fragment>]

    web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece
    web3://0x953d288708bb771f969fcfd9ba0819ef506ac718:1/getTokenHtml/<core>/<tokenId>

The method and its arguments are in the URL, so the player holds no function name. That is
the rule feral-file#3599 states: anything particular to one artist lives in that artist's
contract, and a player that needs to know a function name has already failed.

This is the second scheme the device resolves itself. `docs/onchfs.md` is the first, and the
two share everything below the reader — the source mapping, the offline-cache store, replay,
the media-health exemptions, the disk budget.

## What is and is not guaranteed

`ipfs`, `ar` and `onchfs` addresses commit to their bytes: a reader can refuse a node that
lies, which is why `docs/onchfs.md` can say an RPC node only has to be reachable, never
trusted. **A contract call commits to nothing.** The guarantee is the chain's consensus, and
so the node's.

The device does the two things that are available without a content address, and no more:

- **One pinned block per read.** The block is the first node's head less three, and every
  call of that read — `resolveMode()`, the work itself, and the confirming node's — is made
  at it. Two nodes are asked the same question or the comparison below would mean nothing.
- **A second node's opinion where one answers.** Once a node has answered, the next node in
  the chain's list is asked the identical call at the identical block. Matching bytes mark
  the work `Confirmed`. **Differing bytes fail the read with `ErrDisagree`** — there is
  nothing to decide between them, and showing either would be showing bytes the device
  cannot stand behind.
- **A revert is a disagreement, not a silence.** A node that reverts the call is not a node
  that could not answer: it is saying there is no work at that address, at the same block the
  first node returned bytes for. That is the strongest disagreement available, and reading it
  as "no opinion" is what let one dishonest node serve fabricated bytes while an honest node
  said the work did not exist (round 3 review, F-A).

A second node that cannot answer — unreachable, or behind that block — is **not** a failure.
The work plays on one node and the log says it was not confirmed. A read that required two
would make every web3 work hostage to a second host being up, which is the failure this
whole issue exists to end.

**So the residual risk is a single node lying while every other node for that chain is
unreachable.** It is stated here rather than hidden. The mitigations available are Feral
File's own node being first in the list, and the operator pointing `web3.chains` at a node
they run.

## The pieces

```
dp1 (every exit)      web3://<contract>:<chain>/<method>/<args>?<q>
                        ->  https://<base32 contract>.<chain>.web3.invalid/<method>/<args>?<q>
commandrouter         DeviceLocalMaterializer.Ensure(items, 15 s for the opening work)
offlinecache          materializer: web3.Resolver -> one blob + Automatic ItemRecord
web3                  Resolver -> eth_call at a pinned block -> ABI decode -> confirm
offlinecache replay   Fetch interception serves the record to the kiosk
```

### 1. Source mapping (`contentpath/web3.go`, `dp1/web3.go`)

The kiosk loads http(s) only, so every web3 item leaves `dp1` with a device-local source:

    https://<base32(contract)>.<chainid>.web3.invalid/<method>/<arg>…?<query>#<fragment>

- **The host is a pure function of the contract and the chain**, and the method and
  arguments stay in the path, so `ParseWeb3SourceURL` recovers the exact call from the URL
  alone. Nothing needs a side table.
- **`.invalid` is reserved (RFC 6761)** and never resolves, so a request replay does not
  answer reaches no network.
- **base32** keeps the 20-byte contract inside one DNS label (32 characters, where hex
  would need 40 and the limit is 63).
- **The chain id is always written.** ERC-4804 defaults it to 1, so a URI that omitted it
  and one that wrote `:1` are one work and one cache key.
- **Path, query and fragment are carried byte for byte**, as for onchfs. A `returns=`
  attribute rides in the query, and so does a work's own seed.

**One origin per contract-and-chain, not per work.** Two editions of one contract — Art
Blocks' `getTokenHtml/<core>/<tokenId>` — share an origin, and so share localStorage and
service workers. That is the cost of keeping the host invertible; onchfs can afford an origin
per work because its whole identity is one 32-byte root. Owner's call, feral-file#3599.

**And it is why a web3 record stores its entry URL and nothing else.** A shared origin means
any other URL under it is one a sibling work could also claim, and replay's scope is a single
flat method+URL map across every record in the playlist where the last one wins — so an alias
is a way to serve the wrong work's bytes for a correct URL, silently, since a device-local
source is exempt from every media-health verdict. The origin root is stored only by a work
whose entry *is* the origin root (a URI with no method path). Found in this branch's third
review round (F-B); the consequence is larger than the `localStorage` one the original
decision weighed.

A **name** is not a contract the device reads: `web3://example.eth/artpiece` is refused.
Resolving one would mean a second trusted system (an ENS resolver) in the path of every
work, which is the shape of the problem this issue is about.

The mapping runs at **every call site of `dp1.mapDeviceLocalSources`**, after signature
verification, beside the onchfs mapping (`dp1/devicelocal.go`). That is the rule, stated so it
cannot go stale; the sites today are six, not the four an earlier draft of this document
listed:

| site | what exits there |
|---|---|
| `dp1/dp1.go:172` | `Playlist.UnmarshalJSON` — every decoded document |
| `dp1/dp1.go:420` | `processDynamicPlaylist` — DP-1 hydration and the legacy indexer |
| `dp1/series.go:226` | series expansion |
| `dp1/seriesplayback.go:323` | `CastPlan.Narrow` |
| `dp1/seriesplayback.go:461`, `:471` | `CastPlan.Full` — the other flatten of a plan, taken by the offline-cache download path |
| `dp1/seriesplayback.go:628` | the series cast plan's inherited editions |

**Both `CastPlan` flattens are exits.** `Full` was the one that did not map, found in this
branch's own first review round; the offline-cache key is sha256 of the exact source string,
so a flatten that skips the mapping gives one work two identities.

### 2. Reading (`web3/`)

| step | what |
|---|---|
| `resolveMode()` | ERC-6860's mode. `"manual"` sends the path and query as raw calldata. `"auto"`, an empty answer, or a **revert** is auto mode — neither measured work implements `resolveMode`, so the revert case is the common one. Any other name is `ErrUnsupported`. |
| auto-mode calldata | the first path segment is the method; the rest are arguments, typed by the standard's rules below. |
| `eth_call` | one call, at the pinned block, to one node. |
| decode | without `returns=`, the result is ABI-encoded bytes (a string encodes the same way); with `returns=(…)`, the decoded values are served as JSON. |

Auto-mode argument typing, longest-specific first:

| spelling | type |
|---|---|
| `type!value` | that type, given explicitly |
| `123` | `uint256` |
| `0x` + 64 hex | `bytes32` |
| `0x` + 40 hex | `address` |
| `0x` + even hex | `bytes` |
| anything else | `string` |

**Content type comes from the URL, never from the bytes.** A call carries no header, and
sniffing would let a work choose how it is rendered (an HTML work served as `text/plain`
shows its own source). The last path segment's extension decides, a `returns=` attribute
makes it `application/json`, and the default is `text/html`, which is what these works are.
A method name is not a file name, so `/artpiece` is not parsed for an extension.

The ABI decoder treats every node answer as hostile, as onchfs's does: no length or offset
may exceed the answer it came from, bounds are checked by subtraction, offsets must be word
aligned, and the body is copied out of the answer rather than aliased into it. One exception,
immaterial but stated rather than left wrong: `Plan.Decode`'s shortcut for a result too short
to carry an offset and a length (under 64 bytes) returns that slice of the answer directly.

`MaxWorkBytes` is 16 MiB — about seventy times the largest work measured (Art Blocks'
generator, 224 KiB) — and bounds the hex-encoded RPC answer at roughly 33 MiB.

### 3. Chains and nodes (`web3/resolver.go`)

Ethereum and Base only (`web3.DefaultEndpoints`), the same nodes `docs/onchfs.md` §3 lists
and each checked on 2026-11-13 by reading a whole work through it. ERC-4804 URLs can name any
chain, but **a chain with no node the device has read a work through is a chain it cannot
serve**, and guessing a public RPC for one would ship an unmeasured host of exactly the kind
this issue is about. A URL naming another chain is refused with an error that says so; an
operator adds the chain by listing it.

Within a chain: nodes in order, a node that fails or answers for another chain is cooled for
a minute and the next is tried. `eth_chainId` is checked first, because `rpc.feralfile.com`
answers Base calls from Ethereum. `ErrNotFound` (a revert), `ErrTooLarge` and
`ErrUnsupported` end the attempt — they are answers about the work, not failures of the node.

**Privacy:** a node learns which contracts the device calls, as a gateway would learn which
files it fetched.

Config, the optional `web3` block in `controld.json` (`config.Web3Config`), by the same
salvaging rule as `onchfs`:

```json
"web3": {
  "disabled": false,
  "chains": { "1": { "rpc": ["https://…"] } },
  "requestTimeoutSeconds": 15
}
```

- A chain listed replaces that chain's defaults; other chains keep theirs; a chain the
  defaults do not know is added. Keys are decimal (`"1"`) or CAIP-2 (`"eip155:1"`).
- Entries are salvaged one by one: https only, no userinfo, query or fragment.
- A block that does not decode is the defaults, except an explicit `"disabled": true`.
- **Unlike onchfs, these nodes are trusted** (see *What is and is not guaranteed*). Editing
  this list changes what the device will show, not only where it looks.

### 4. Into the offline cache, out through replay (`offlinecache/web3.go`)

A web3 work is one document, not a tree, so there is one blob and three URLs for it:

- the **exact source URL**, query included, as the entry — replay matches method and URL
  exactly, and the kiosk navigates to that string;
- the **origin root** (`https://<host>/`), which is what a relative `/` resolves to;
- the **path without the query**, for a document that links to itself by path.

Each carries the declared content type and `Access-Control-Allow-Origin: *`, which the
player's cross-origin HEAD probe needs. None carries `Content-Encoding`.

Everything else about the record is the onchfs path unchanged, because it is literally the
same code (`offlinecache/devicelocal.go`): `Automatic` so it stays out of the app's Downloads
list, fresh for 30 days and rebuilt within three days of expiry, published under the store's
publish guard so a GC cannot sweep its blob, refused if a clear raced it, and evicted
oldest-played-first when the disk is full. **The two schemes share one retention pool and one
LRU order**, because they share one disk: a web3 work nothing has played is the right thing to
drop to make room for an onchfs work on screen.

`ClassWeb3` is classified without a request, ahead of the source guard, which would refuse
the unresolvable host. Downloads and automatic warming skip it as they skip `data:` and
onchfs.

### 5. Media health

A web3 source has no origin, so `mediahealth.IsDeviceHeld` covers it exactly as it covers
`data:` and onchfs: the L0 probe, the kiosk navigation observer, inferred play failures and
the L1 render probe all skip it, and `Service.applyEntry` records no player outcome for it. An
RPC outage can never mark a web3 work broken, and the player is free to skip one it cannot
load.

## Known limitations

- **A sub-resource misses, and so does a self-reference.** The call returns one document; a
  work whose markup pulls a relative sub-resource has nothing to pull it from, and an absolute
  URL to another origin leaves the replay scope and goes to the network as it would from any
  page. A work that references *itself* by `/` or by its bare path misses too, deliberately:
  on a shared origin those URLs belong to whichever sibling work is the root or carries that
  exact path (§1). It fails closed at a `.invalid` host rather than open onto another work.
  Both measured works are self-contained single documents, which is what a work behind a view
  function has to be.
- **A first play needs the network and a reachable node for that chain.** A work that cannot
  be read stays unplayable and the player skips it like any failed source.
- **A lying node is the residual risk**, bounded as described above. There is no content
  address, so this cannot be closed, only narrowed.
- **A name service is not resolved.** A `web3://` URL naming an ENS name is refused.
- **No upstream producer emits `web3://` yet.** Neither `ff-indexer-v2` nor
  `art-catalog-service` carries a `web3://` source, so today these works reach a device by a
  cast of a DP-1 playlist that names one. Making the fleet play Han's works from the catalog
  is a cross-repo follow-up.

## Verification

- Unit tests:
  - `contentpath`: URI forms, look-alike hosts, the mapping round trip, the shared
    device-local predicate.
  - `web3`: the calldata and decode of a recorded Ethereum answer
    (`testdata/digital-soul.json`); auto-mode typing including Art Blocks' two arguments;
    manual mode; hostile ABI answers; `returns=`; resolver failover, the chain-id check, the
    second opinion matching, disagreeing and absent; `MergeEndpoints` salvage.
  - `offlinecache`: materializer to replay, end to end through `Fetch.fulfillRequest`; one
    retention pool across both schemes; a disabled scheme not taking the other down.
  - `dp1`: every exit. `commandrouter`: the open wait. `mediahealth`, `config`.
- **Not verified on hardware.** The issue's "Check when done" needs a Kitchen unit on FF OS
  2.0.11 and a 3.0.0 unit with `onchfs.fxhash2.xyz` and every Infura host blocked at the
  router. That acceptance is outstanding.
