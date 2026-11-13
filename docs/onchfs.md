# onchfs works on the FF1

Status: shipped in feral-controld (ffos-user#427). Reader package: `components/feral-controld/onchfs`.

## Why

onchfs is the on-chain file system fxhash wrote generative works into, on Ethereum, Base
and Tezos. A work is addressed `onchfs://<root>[/path][?query]`, where the root is the
keccak address of its root inode. fxhash closed on 2026-08-17, and with it went
`onchfs.fxhash2.xyz`, the only HTTP gateway that ever served onchfs. The works are intact
in chain state.

ff-indexer-v2 (#180) now stores and serves the work's own `onchfs://<root>/?fxhash=…` URI,
the same string as `artifactUri` in its on-chain metadata, and runs no gateway. Players
read from chain. This document is the FF1's side of that.

The rule, as for IPFS and Arweave: the device keeps only the content address (root, path,
query) and decides where the bytes come from. For onchfs the bytes come from the chain,
through public nodes the device does not trust. Every inode is verified against its
address before anything is stored or shown.

## The pieces

```
dp1 (every exit)          onchfs://<root>/?fxhash=…  ->  https://<base32 root>.onchfs.invalid/?fxhash=…
commandrouter (cast)      OnchfsMaterializer.Ensure(items, 15 s for the first work)
offlinecache              materializer: onchfs.Resolver -> verified tree -> blobs + Automatic ItemRecord
onchfs                    Resolver -> chain readers (EVM eth_call / Tezos run_script_view) -> verify
offlinecache replay       Fetch interception serves the record to the kiosk, as for any cached work
```

### 1. Source mapping (`contentpath/onchfs.go`, `dp1/onchfs.go`)

The kiosk loads http(s) only, so every onchfs item leaves `dp1` with a device-local source:

    https://<base32(root)>[.<chain>].onchfs.invalid/<path>?<query>#<fragment>

- **One host per root**, so one work's storage and service workers are never another's.
- **`.invalid` is reserved (RFC 6761)** and never resolves, so a request replay does not
  answer reaches no network.
- **base32** keeps the 32-byte root inside one DNS label: 52 characters, where hex would
  need 64 and the label limit is 63.
- **Path, query and fragment are carried byte for byte.** fxhash works read their seed from
  the query.
- **The chain label carries what is known about the chain.** An explicit CAIP-2
  authority (`onchfs://eip155:1/<root>`) gives `.eth`, `.base` or `.tezos`. Otherwise
  the item's provenance gives the family: `.evm` for an EVM token, `.tezos` for a
  Tezos one. With neither, there is no label. The family is a security input; see §3.

The mapping is pure and idempotent, so the offline cache, which keys a work by sha256 of its
exact source, sees one key per work. It is applied at every exit an item can take from
`dp1`:

- `Playlist.UnmarshalJSON` (every decoded document);
- `processDynamicPlaylist` (DP-1 hydration, the Raster walk, the legacy indexer);
- series expansion and `CastPlan.Narrow` (catalog editions).

It runs after signature verification, which covers the document's original bytes. A
source on fxhash's dead gateway (`https://onchfs.fxhash2.xyz/<root>…`) is read as the URI it
came from, as the indexer does at ingest. That host is never dialed.

`rasterSourceURL` no longer maps onchfs to a host. The gateway table shared with ff-app and
art-catalog-service keeps `ipfs` and `ar` only. For onchfs, the cross-repo rule is that the
URI passes through unchanged and each player resolves it itself.

### 2. Reading and verifying (`onchfs/`)

The two chains derive a directory's address the same way:
`keccak(0x00 ‖ for each entry, names descending: checksum ‖ keccak(name))`.
They derive a file's address differently:

| Chain | File address | So the reader… |
|---|---|---|
| Ethereum, Base (`FileSystem.sol`) | `keccak(0x01 ‖ keccak(chunk checksums) ‖ keccak(metadata))` | proves the chunk list, then proves the content is exactly those chunks (equal slices first, each chunk fetched alone if they do not match) |
| Tezos (`KT1Ae7dT1gsLw2tRnUMXSCmEyF74KVkM6LUo`) | `keccak(0x01 ‖ keccak(content) ‖ keccak(metadata))` | proves the content directly; `read_file` returns it whole |

The Tezos formula was measured on two mainnet trees (`testdata/tezos-*.json`). **The formula
is chosen by chain, never by trying both.** On EVM the content form is met by a node that
answers `readFile` with the packed chunk checksums, which passes forged bytes under a valid
address (#428 review F5).

File metadata is an HPACK header block. `Content-Type` comes from it, never from sniffing,
and a body is gunzipped only when its verified `content-encoding` says gzip.

The ABI decoder treats every node answer as hostile:
- no value may exceed the answer it came from;
- bounds are checked by subtraction;
- content is copied out of the answer;
- directory names together may not exceed the answer.

`Limits` bound one tree read:
- 64 MiB per file, stored or decompressed;
- 64 MiB kept across distinct inodes;
- 8,192 paths, directories included;
- 4,096 entries per directory;
- depth 16;
- 4 reads in flight.

An inode named twice is read once and shared, so aliasing costs nothing.

### 3. Chains and nodes (`onchfs/resolver.go`)

An onchfs URI usually names no chain. Which chains are asked is a security rule, not a
latency choice (PR #461 review F1).

Every EVM file address is also a valid Tezos address for bytes equal to that file's packed
chunk checksums: Tezos hashes content, EVM hashes the chunk list. Racing all chains would
let one forged Tezos node pass those bytes off as an Ethereum work. The reverse forgery
needs a keccak preimage. So:

- **Explicit authority:** that chain only.
- **Family from provenance:** `.evm` asks the EVM chains only, `.tezos` Tezos only.
- **Nothing known:** the EVM chains first. Tezos is asked only after every EVM chain
  confirms the root is absent with two nodes (one when a chain has a single node), so a
  forged Tezos answer needs a lying EVM node as well. A node that denies a root another
  node serves is logged and cooled.

Within a chain:
- Nodes are tried in order. A read comes from one node, pinned three blocks below its head,
  after checking `eth_chainId` or the Tezos `chain_id`. `rpc.feralfile.com` answers Base
  calls from Ethereum, so this check is necessary.
- A node that fails, lies or is on another chain is cooled for a minute and the next is
  tried.
- `ErrNotFound` and `ErrTooLarge` end the attempt: they are answers about the work, not the
  node.
- Callers refuse redirects, time out after 15 s and cap answers.

Default nodes (`onchfs.DefaultEndpoints`). Each was checked on 2026-11-13 by reading a
whole work through it (for Base, a verified not-found):

| Chain | Nodes |
|---|---|
| `eip155:1` | `rpc.feralfile.com`, `ethereum-rpc.publicnode.com`, `eth.drpc.org` |
| `eip155:8453` | `mainnet.base.org`, `base-rpc.publicnode.com`, `base.drpc.org` |
| `tezos:NetXdQprcVkpaWU` | `mainnet.smartpy.io`, `rpc.tzkt.io/mainnet` |

These were refused at that check:
- `eth.llamarpc.com` and `cloudflare-eth.com` returned errors;
- `rpc.ankr.com` needs a key;
- `1rpc.io` and `eth.merkle.io` rate-limited a single work;
- `rpc.tzbeta.net` answers 401 to `run_script_view`;
- ECAD's Tezos nodes no longer resolve.

**Privacy:** a node learns which roots the device reads, as a gateway would. Feral File's
own node is first where one exists.

Config, the optional `onchfs` block in `controld.json` (`config.OnchfsTuning`):

```json
"onchfs": {
  "disabled": false,
  "chains": { "eip155:1": { "rpc": ["https://…"] } },
  "requestTimeoutSeconds": 15
}
```

- A chain listed replaces that chain's defaults; other chains keep theirs.
- Entries are salvaged one by one: https only, no userinfo, query or fragment. Bad ones are
  dropped and logged.
- A block that does not decode is the defaults, except an explicit `"disabled": true`, which
  is honored.
- Add a node only after a whole work has been read through it from a device.

### 4. Into the offline cache, out through replay (`offlinecache/onchfs.go`)

`OnchfsMaterializer` is the cache's second producer of item records. The first is capture,
which needs a live host. The materializer resolves the tree and writes each file's plain
bytes as a blob. It then saves a record under the device-local source with these resources:

- each file at `<origin>/<path>`, spelled as a browser requests it (WHATWG path
  percent-encoding, so `!` stays literal), plus fallback spellings (`^` and `|` escaped,
  and Go's full escaping) wherever no other file's own spelling is;
- each directory with an `index.html` at `<origin>/<dir>/`, and the root at `<origin>/`;
- the exact source URL, query included, as the entry. A directory named without its slash
  is a 301 to the slashed form, query kept.

Every resource carries the verified `Content-Type` (or none) and
`Access-Control-Allow-Origin: *`, which the player's cross-origin HEAD probe needs. None
carries `Content-Encoding`.

Records are `Automatic`, which keeps them out of the app's Downloads list. They stay fresh
for 30 days and are rebuilt on the next cast that needs them. They are rebuilt early when
within three days of expiry.

The cast path (`commandrouter`) calls `Ensure` after the content-policy filter and before the
preflight, outside both locks:
- It waits up to 15 s for the first onchfs work in playlist order; the rest land in the
  background.
- Each background work that lands calls `OnReady`, which forces a refresher pass that widens
  the replay scope.
- The refresher starts any onchfs work a refresh brings, without waiting.

Replay then serves the work like any cached one. Measured on public nodes, a whole work
reads in 1.5–3.5 s (Casey Reas, *There's No Distance #1*: five files, 1 MB, 3.4 s). After
its first play a work is available offline.

**With the offline cache feature off** (the default), `main.go` builds `BootstrapCore`
lazily: store and replay, with no Service, no static server and no capture.
- The kiosk is not dialed until an onchfs scope is first armed, so a device that never shows
  an onchfs work holds no extra socket.
- The app's offline-cache commands still answer "not enabled".
- Nothing else evicts in this mode, so the materializer keeps its own records under 512 MiB,
  oldest played first, and collects their blobs.

**With the feature on**, the cache's own disk policy bounds these records, and the
materializer never runs GC.

**Publication and GC.** GC is mark-and-sweep, and a GC that marks before a record is
saved and sweeps after deletes its new blobs; a check after the save cannot see a sweep
that has not happened yet (PR #461 review F3). So the store `BootstrapCore` builds is a
publish guard: GC takes it exclusively, and the materializer writes its blobs and saves
its record while holding it shared. `fresh` also checks that every blob a record names is
on disk, so a damaged record is rebuilt on the next cast instead of reported ready.

**A full cache.** The cache shares one disk budget, so when it refuses a new work's
blobs, older onchfs works make room, least recently played first. A work the current
playlist shows is never evicted. Standalone pruning keeps those works too, even past
512 MiB; that budget is a retention bound, and the cache's disk budget still caps the
disk. With a Service running, eviction goes through its
`ClearItem`, whose GC is serialized with captures; standalone, the materializer deletes
and collects itself (PR #461 review F6).

A root that is a single file is its own entry. A directory entry without its slash is a
301 whose target, query included, is stored as well.

`ClassOnchfs` is classified without a request, ahead of the source guard, which would refuse
the unresolvable host. Downloads and automatic warming skip it as they skip `data:`.

### 5. Media health

An onchfs source has no origin. `mediahealth.IsDeviceHeld` covers `data:` and onchfs, and
these paths skip it:
- the L0 probe (inconclusive, never recorded);
- the kiosk navigation observer;
- inferred play failures;
- the L1 render probe.

So an RPC outage can never mark an onchfs work broken. The bytes are verified when they are
read.

## Known limitations

- **A sub-resource requested with its own query string** (`lib.js?v=2`) misses: replay
  matches the exact URL, and the tree has no such URL. Not seen in the recorded works.
- **A first play needs the network and a reachable node per chain.** A work that cannot be
  read stays unplayable, and the player skips it like any failed source.
- **An absent root is reported by the first node that answers.** A lying node can make a work
  look absent until the next cast. It cannot make one show wrong bytes.
- **ff-app and art-catalog-service** still map onchfs to the dead gateway. They must pass the
  URI through and resolve it themselves (follow-ups outside this repository).
- **The app sees device-local sources in `player_status`.** The offline-cache download and
  clear commands accept the raw `onchfs://` URI. Download matches the playlist item on the
  same work, whatever its chain label. Clear tries each form the work can be stored under
  (`dp1.OnchfsSourceVariants`).

## Verification

- Unit tests:
  - `contentpath`: URI forms and the mapping round trip.
  - `onchfs`: recorded Ethereum and Tezos trees; forgeries; hostile ABI; limits; resolver
    failover.
  - `offlinecache`: materializer to replay, end to end through `Fetch.fulfillRequest`; lazy
    core.
  - `dp1`: every exit.
  - `commandrouter`: order, and source mapping.
  - `playlist-refresher`, `mediahealth`, `config`.
- Live, against default nodes (2026-11-13): the Casey Reas tree on Ethereum and two Tezos
  trees resolve and verify, and a missing root is not-found on all three chains in 1 s.
- On a device: cast an Ethereum and a Tezos onchfs work. Check `location.search` carries
  the seed, sub-resources load, and a second play works with the WAN unplugged. Do this
  before `develop → staging`.
