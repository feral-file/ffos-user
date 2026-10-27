package dp1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goio "io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Raster wallet walk (feral-file/feral-file#3576, step 6).
//
// ff-app casts an address playlist as a DP-1 `dynamicQuery` whose items are
// not DP-1 items but Raster `tokensOwned` nodes, paged by cursor, with the
// wallet address inlined in the query text. dp1-go's hydration cannot run it:
// its typed DynamicQuery drops `pagination`, its loop pages by `{{limit}}`/
// `{{offset}}` placeholders this query does not carry, and it validates every
// node as a DP-1 item, which a Raster node is not. This file is the device's
// own resolver for that one shape, keyed on `responseMapping.itemSchema`.
//
// The mapping rules mirror ff-app's RasterTokenMapper.toPlaylistItem and
// art-catalog-service's rasterTokenToIndexerToken so the phone and the wall
// build the same work from the same answer. Where this file and those
// diverge, one of them is wrong. Two things in particular must stay aligned
// byte for byte: the gateway table (the offline cache keys a work by its
// exact `source` string, so a different gateway is a second cache entry) and
// the item id inputs (the legacy indexer path's UUID inputs, so ids stay
// stable across the indexer -> Raster switch).

const (
	// RasterTokensOwnedItemSchema is the responseMapping.itemSchema that selects
	// this resolver. `dp1/*` stays on dp1-go; any other schema is refused.
	RasterTokensOwnedItemSchema = "raster/tokensOwned/1.0" //nolint:gosec // a schema name, not a credential

	// rasterAPIKeyHeader carries the device's own Raster read key. It is never
	// read from the document: the playlist is cast and may be published.
	rasterAPIKeyHeader = "x-api-key"

	// rasterMaxPages caps one walk. At Raster's 250-per-page cap that is 50,000
	// holdings, far past any wallet a wall should try to play; hitting it is an
	// error, not a truncation, so a cursor loop that never terminates (a server
	// bug, or a hostile endpoint in debug mode) cannot spin the daemon.
	rasterMaxPages = 200

	// rasterMaxPageBytes bounds one page body. A full page of 250 nodes with
	// the fields the walk asks for is well under 1 MiB.
	rasterMaxPageBytes = 16 << 20

	// indexerTezosChain is the chain string the legacy indexer path fed
	// generateTokenUUID for a Tezos token (ff-indexer-v2 `tezos.chain_id`).
	// Raster spells the same network `tezos:NetXdQprcVkpaWU`; the id must not
	// change with the spelling.
	indexerTezosChain = "tezos:mainnet"

	// Feral File moved artwork files off this host without changing paths;
	// Raster still serves the pre-move snapshot for some works
	// (feral-file/feral-file#3546). Only the host prefix is rewritten.
	retiredFeralFileCDN = "https://cdn.feralfileassets.com/"
	feralFileCDN        = "https://cdn.artworks.feralfile.io/"

	// Gateway table, decision 12 of ff-app's Raster plan. `ipfs.io` is retired
	// and must not reappear here.
	gatewayIPFS          = "https://ipfs.raribleuserdata.com/ipfs/"
	gatewayIPFSFeralFile = "https://ipfs.feralfile.com/ipfs/"
	gatewayArweave       = "https://arweave.net/"
	gatewayOnchfs        = "https://onchfs.fxhash2.xyz/"

	// feralFilePlatform is Raster's `platform.name` for Feral File works,
	// compared case-insensitively. Feral File pins its own works, so its
	// gateway answers for them where a public one may not.
	feralFilePlatform = "feral file"

	rasterChainEVM   = "evm"
	rasterChainTezos = "tezos"
)

// rasterHosts is the allowlist a Raster walk's endpoint must be on. The
// endpoint arrives inside a controller-supplied document, and the walk
// attaches the device's own API key to every request, so an unrestricted
// host would let a crafted playlist both aim the device's queries at an
// arbitrary server and hand it the key. Same posture as ffindexer's
// FF_INDEXER_HOSTS for the legacy path. Relaxed only by controld --debug
// (httptest servers), the same switch that relaxes dp1-go's endpoint policy.
var rasterHosts = map[string]bool{
	"api.raster.art": true,
}

// NewRasterHTTPClient builds the client the Raster walk sends the device key
// through: the daemon's usual whole-request timeout, and NO redirect
// following. A 3xx comes back as the response it is, fetchRasterPage reports
// it as a failed page, and the key never leaves the allowlisted host. Pinned
// by TestNewRasterHTTPClient_DoesNotFollowRedirects.
func NewRasterHTTPClient() wrapper.HTTPClient {
	return wrapper.NewHTTPClientFrom(&http.Client{
		Timeout: wrapper.HTTPClientTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})
}

// ErrRasterQuery marks a Raster walk the device cannot or will not run:
// a malformed document (missing pagination, wrong profile, disallowed
// endpoint), a page that failed, or a walk past the page cap. Content from
// Raster is never partially kept: any page failure fails the resolve.
var ErrRasterQuery = errors.New("raster dynamicQuery")

var (
	eip155ChainRE     = regexp.MustCompile(`^eip155:(\d+)$`)
	tezosChainRE      = regexp.MustCompile(`^tezos(:[a-z0-9]+)?$`)
	evmAddressRE      = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	decimalRE         = regexp.MustCompile(`^[0-9]+$`)
	artBlocksRE       = regexp.MustCompile(`(?i)art ?blocks`)
	protocolURIRE     = regexp.MustCompile(`(?is)^(ipfs|ar|onchfs)://(.*)$`)
	ipfsDoublePrefixR = regexp.MustCompile(`(?i)^ipfs/`)
	// artBlocksProjectSpan: an Art Blocks token id is project * 1,000,000 + mint.
	artBlocksProjectSpan = big.NewInt(1_000_000)
)

// evmChainLabels mirrors ff-app's ChainRegistry labels for the title
// fallback `<chain label> <contract…> #<tokenId>`. An EVM network not listed
// here is labeled "EVM chain <n>", as the app does, rather than dropped.
var evmChainLabels = map[string]string{
	"1":       "Ethereum",
	"8453":    "Base",
	"10":      "Optimism",
	"137":     "Polygon",
	"42161":   "Arbitrum",
	"360":     "Shape",
	"7777777": "Zora",
}

// Raster wire shapes, the subset of `tokensOwned.nodes[]` the mapper reads.
// Scalars that Raster may send as a number or a string (`id`, `tokenId`) are
// held raw and read through rasterScalar.
type rasterOwnedNode struct {
	// LastAcquiredAt is when the walked address last acquired the token, the
	// primary sort key (see sortRasterItems).
	LastAcquiredAt string       `json:"lastAcquiredAt"`
	Token          *rasterToken `json:"token"`
}

// rasterWalkItem is a mapped item with the two keys the walk orders by.
type rasterWalkItem struct {
	item dp1playlist.PlaylistItem
	// acquiredUs is lastAcquiredAt in microseconds since the epoch, 0 when
	// Raster has no date or sends one that does not parse.
	acquiredUs int64
	// workID is the app's work id (CID) for the token, the tie-break.
	workID string
}

// rasterAcquiredUs mirrors the app's sortKeyUs: `DateTime.tryParse(...)
// .microsecondsSinceEpoch`, 0 when absent or unparsable. Microseconds, not
// nanoseconds, so two acquisitions the app sees as a tie are a tie here too.
func rasterAcquiredUs(value string) int64 {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return t.UnixMicro()
}

// rasterWorkIDStandard mirrors ff-app's tokenStandardSegment. It is the
// work-id spelling, which is NOT the DP-1 provenance enum
// (rasterTokenStandard): CryptoPunks and ordinals keep their own segment
// here, and a hyphenated standard is `other` exactly as the app reads it.
func rasterWorkIDStandard(standard string) string {
	switch strings.ToUpper(strings.TrimSpace(standard)) {
	case "ERC721":
		return "erc721"
	case "ERC1155":
		return "erc1155"
	case "FA2":
		return "fa2"
	case "CRYPTOPUNKS":
		return "cryptopunks"
	case "ORDINAL_INSCRIPTION":
		return "ordinal"
	default:
		return "other"
	}
}

// sortRasterItems puts a walk in the order the app shows an address
// playlist: newest acquisition first, ties broken by work id descending
// (ff-app app_database.dart orders playlist entries by sortKeyUs DESC, then
// itemId DESC, and an address playlist's itemId is the work id).
//
// Raster also returns newest acquisition first, but breaks ties by its own
// token id ascending, and that tie rule is observed behavior, not a documented
// contract. Ties are common, not an edge case: one probed wallet had 332 of
// its 595 tokens sharing a timestamp with another token (a whole mint or
// transfer batch lands in one block). Sorting here makes the wall's order a
// function of the wallet alone, identical to the phone's, whatever order a
// page comes back in. Stable, so two items with the same work id (which
// cannot happen in one wallet) would still keep a defined order.
func sortRasterItems(items []rasterWalkItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].acquiredUs != items[j].acquiredUs {
			return items[i].acquiredUs > items[j].acquiredUs
		}
		return items[i].workID > items[j].workID
	})
}

type rasterToken struct {
	ID              json.RawMessage `json:"id"`
	ChainID         string          `json:"chainId"`
	ContractAddress string          `json:"contractAddress"`
	TokenID         json.RawMessage `json:"tokenId"`
	TokenStandard   string          `json:"tokenStandard"`
	Name            string          `json:"name"`
	Media           *rasterMedia    `json:"media"`
	Artworks        []rasterArtwork `json:"artworks"`
}

type rasterMedia struct {
	ContentURL           string `json:"contentUrl"`
	NormalizedContentURL string `json:"normalizedContentUrl"`
}

type rasterArtwork struct {
	Title    string `json:"title"`
	Platform *struct {
		Name string `json:"name"`
	} `json:"platform"`
}

func (t *rasterToken) primaryArtwork() *rasterArtwork {
	// A token occasionally belongs to several artworks; the first is Raster's
	// primary association (same reading as the app and the catalog).
	if t == nil || len(t.Artworks) == 0 {
		return nil
	}
	return &t.Artworks[0]
}

func (t *rasterToken) platformName() string {
	artwork := t.primaryArtwork()
	if artwork == nil || artwork.Platform == nil {
		return ""
	}
	return strings.TrimSpace(artwork.Platform.Name)
}

// rasterScalar reads a JSON scalar Raster may send as a string or a number.
// null and absent read as "".
func rasterScalar(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	return string(trimmed)
}

// rasterChain is what the mapper needs to know about a token's network.
type rasterChain struct {
	// kind is the DP-1 provenance chain: evm or tezos.
	kind string
	// indexerChain is the chain string in the legacy indexer's spelling, the
	// generateTokenUUID input: `eip155:<n>` as is, Tezos as `tezos:mainnet`.
	indexerChain string
	label        string
}

// lookupRasterChain mirrors ff-app's ChainRegistry.lookup: `tezos`,
// `tezos:mainnet` and `tezos:NetXdQprcVkpaWU` are all Tezos mainnet; any
// `eip155:<n>` is an EVM chain; anything else is unsupported and the token is
// skipped, exactly as the app drops a token it cannot give a work id.
func lookupRasterChain(id string) (rasterChain, bool) {
	key := strings.ToLower(strings.TrimSpace(id))
	if tezosChainRE.MatchString(key) {
		return rasterChain{kind: rasterChainTezos, indexerChain: indexerTezosChain, label: "Tezos"}, true
	}
	if m := eip155ChainRE.FindStringSubmatch(key); m != nil {
		label := evmChainLabels[m[1]]
		if label == "" {
			label = "EVM chain " + m[1]
		}
		return rasterChain{kind: rasterChainEVM, indexerChain: key, label: label}, true
	}
	return rasterChain{}, false
}

// rasterNodeToItem maps one `tokensOwned` node to a DP-1 item. A non-empty
// reason means the node is skipped: the wall cannot play it, and the caller
// logs the reason with the Raster token id and goes on. The app keeps such a
// token as a bare row so wallet counts match the chain; a wall has nothing to
// show for it.
func rasterNodeToItem(node rasterOwnedNode) (dp1playlist.PlaylistItem, string) {
	walkItem, reason := rasterNodeToWalkItem(node)
	return walkItem.item, reason
}

func rasterNodeToWalkItem(node rasterOwnedNode) (rasterWalkItem, string) {
	token := node.Token
	if token == nil {
		return rasterWalkItem{}, "no token"
	}
	chain, ok := lookupRasterChain(token.ChainID)
	if !ok {
		return rasterWalkItem{}, fmt.Sprintf("unsupported chain %q", token.ChainID)
	}
	contract := strings.TrimSpace(token.ContractAddress)
	if contract == "" {
		return rasterWalkItem{}, "no contract address"
	}
	if chain.kind == rasterChainEVM && !evmAddressRE.MatchString(contract) {
		return rasterWalkItem{}, "malformed EVM contract address"
	}
	tokenID := rasterScalar(token.TokenID)
	if tokenID == "" {
		return rasterWalkItem{}, "no token id"
	}
	source := rasterSourceURL(token.Media, token.platformName())
	if source == "" {
		return rasterWalkItem{}, "no playable media URL"
	}

	// Id parity with the legacy indexer path (buildPlaylistItem): ff-indexer-v2
	// stores an EVM contract address EIP-55 checksummed (its NormalizeAddress
	// is go-ethereum's HexToAddress().String()) and a Tezos address verbatim,
	// and names the chain `eip155:1` / `tezos:mainnet`. Feeding the same three
	// strings keeps the UUID a token had under the indexer.
	indexerContract := contract
	provenanceAddress := contract
	if chain.kind == rasterChainEVM {
		indexerContract = common.HexToAddress(contract).Hex()
		provenanceAddress = strings.ToLower(contract)
	}
	duration := float64(DEFAULT_DURATION)
	item := dp1playlist.PlaylistItem{
		ID:       generateTokenUUID(indexerContract, chain.indexerChain, tokenID),
		Title:    rasterTitle(token, chain, contract, tokenID),
		Source:   source,
		Duration: &duration,
		License:  "open",
		Provenance: &dp1playlist.ProvenanceBlock{
			Type: dp1playlist.ProvenanceOnChain,
			Contract: &dp1playlist.ProvenanceContract{
				Chain:    chain.kind,
				Standard: rasterTokenStandard(token.TokenStandard),
				Address:  provenanceAddress,
				TokenID:  tokenID,
			},
		},
	}
	if err := validateMappedItem(item); err != nil {
		return rasterWalkItem{}, "mapped item invalid: " + err.Error()
	}
	return rasterWalkItem{
		item:       item,
		acquiredUs: rasterAcquiredUs(node.LastAcquiredAt),
		// The app's cidForRasterToken: `<namespace>:<standard>:<contract>:
		// <tokenId>`, EVM contract lowercased, Tezos verbatim, Tezos named
		// `tezos:mainnet` like the legacy indexer.
		workID: chain.indexerChain + ":" + rasterWorkIDStandard(token.TokenStandard) + ":" + provenanceAddress + ":" + tokenID,
	}, ""
}

// rasterTokenStandard lowercases Raster's `tokenStandard` (`ERC721`, `ERC-1155`,
// `FA2`) into the DP-1 provenance enum; anything else is `other`.
func rasterTokenStandard(standard string) string {
	switch s := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(standard)), "-", ""); s {
	case "erc721", "erc1155", "fa2":
		return s
	default:
		return "other"
	}
}

// rasterTitle applies the app's title rules: the token's own name; else the
// first artwork's title, with ` #<mint>` for Art Blocks (Raster leaves Art
// Blocks names empty and only Art Blocks encodes the mint in the token id);
// else `<chain label> <contract…> #<tokenId>` so the row is still recognizable.
func rasterTitle(token *rasterToken, chain rasterChain, contract, tokenID string) string {
	if name := strings.TrimSpace(token.Name); name != "" {
		return name
	}
	if artwork := token.primaryArtwork(); artwork != nil {
		if title := strings.TrimSpace(artwork.Title); title != "" {
			if mint, ok := artBlocksMint(token.platformName(), tokenID); ok {
				return title + " #" + mint
			}
			return title
		}
	}
	return chain.label + " " + shortContract(contract) + " #" + tokenID
}

// artBlocksMint is `tokenId mod 1,000,000` for an Art Blocks token, read off
// the decimal token id. A Feral File token id is a hash and means nothing as
// a number, so no other platform gets one.
func artBlocksMint(platform, tokenID string) (string, bool) {
	if !artBlocksRE.MatchString(platform) || !decimalRE.MatchString(tokenID) {
		return "", false
	}
	id, ok := new(big.Int).SetString(tokenID, 10)
	if !ok {
		return "", false
	}
	return new(big.Int).Mod(id, artBlocksProjectSpan).String(), true
}

// shortContract: `0xbb12…9cec` for an EVM contract, `KT1K…HaCE` for Tezos;
// the whole value when it is too short to elide.
func shortContract(contract string) string {
	if len(contract) <= 12 {
		return contract
	}
	return contract[:6] + "…" + contract[len(contract)-4:]
}

// rasterSourceURL makes a Raster content URL playable, or returns "" when there
// is nothing a wall can open. `normalizedContentUrl` wins over `contentUrl`
// (first non-blank). Order: retired-host rewrite, then `ipfs://`, `ar://` and
// `onchfs://` through the gateway table (a doubled `ipfs://ipfs/<cid>` prefix
// is collapsed; path and query are kept, which matters for fxhash `?fxhash=`
// seeds). Plain http(s) is returned exactly as given, never re-serialized, so
// host case and percent-encoding stay as Raster sent them: the same string the
// app and the catalog store, which is what the offline cache keys on. Any other
// scheme (`data:`, a bare CID, a proprietary URL) is "".
func rasterSourceURL(media *rasterMedia, platform string) string {
	if media == nil {
		return ""
	}
	raw := strings.TrimSpace(media.NormalizedContentURL)
	if raw == "" {
		raw = strings.TrimSpace(media.ContentURL)
	}
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, retiredFeralFileCDN) {
		raw = feralFileCDN + raw[len(retiredFeralFileCDN):]
	}
	m := protocolURIRE.FindStringSubmatch(raw)
	if m == nil {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		return raw
	}
	scheme := strings.ToLower(m[1])
	rest := m[2]
	if scheme == "ipfs" {
		rest = ipfsDoublePrefixR.ReplaceAllString(rest, "")
	}
	if rest == "" {
		return ""
	}
	switch scheme {
	case "ipfs":
		if strings.EqualFold(strings.TrimSpace(platform), feralFilePlatform) {
			return gatewayIPFSFeralFile + rest
		}
		return gatewayIPFS + rest
	case "ar":
		return gatewayArweave + rest
	default:
		return gatewayOnchfs + rest
	}
}

// validateMappedItem checks a mapped item against the constraints dp1-go's
// item schema puts on the fields this mapper fills: `id` is a UUID, `source`
// is an absolute URI, `duration` is non-negative, `license` is a known mode.
// dp1-go exposes its item validator only inside its hydration loop (and the
// whole-document validators in its root package), so the device checks the
// same contract here. By construction every mapped item passes; the check is
// the backstop that keeps a mapper regression from putting an item on the
// wall that dp1-go would have refused.
func validateMappedItem(item dp1playlist.PlaylistItem) error {
	if _, err := uuid.Parse(item.ID); err != nil {
		return fmt.Errorf("id %q is not a UUID", item.ID)
	}
	u, err := url.Parse(item.Source)
	if err != nil || !u.IsAbs() || u.Host == "" || strings.ContainsAny(item.Source, " \t\r\n") {
		return fmt.Errorf("source is not an absolute URI")
	}
	if item.Duration == nil || *item.Duration < 0 {
		return fmt.Errorf("duration must be non-negative")
	}
	switch item.License {
	case "open", "token", "subscription":
	default:
		return fmt.Errorf("license %q is not a DP-1 license mode", item.License)
	}
	return nil
}

// rasterWalkPlan is the validated request plan for one walk: the document's
// query and endpoint, with the pagination contract checked up front so a
// malformed document is refused before any request is made.
type rasterWalkPlan struct {
	endpoint   string
	query      string
	headers    map[string]string
	itemsPath  string
	pagination DynamicQueryPagination
}

func (d *dp1) planRasterWalk(playlist Playlist) (rasterWalkPlan, error) {
	dq := playlist.DynamicQuery
	if dq.Profile != dp1playlist.ProfileGraphQLV1 {
		return rasterWalkPlan{}, fmt.Errorf("%w: %s requires profile %q, got %q",
			ErrRasterQuery, RasterTokensOwnedItemSchema, dp1playlist.ProfileGraphQLV1, dq.Profile)
	}
	if dq.Method != "" && !strings.EqualFold(dq.Method, http.MethodPost) {
		return rasterWalkPlan{}, fmt.Errorf("%w: graphql-v1 expects POST, got %q", ErrRasterQuery, dq.Method)
	}
	if strings.TrimSpace(dq.Query) == "" {
		return rasterWalkPlan{}, fmt.Errorf("%w: empty query", ErrRasterQuery)
	}
	if strings.TrimSpace(dq.ResponseMapping.ItemsPath) == "" {
		return rasterWalkPlan{}, fmt.Errorf("%w: responseMapping.itemsPath is required", ErrRasterQuery)
	}
	pag := playlist.DynamicQueryPagination
	if pag == nil || pag.Variable == "" || pag.CursorPath == "" || pag.HasNextPath == "" {
		return rasterWalkPlan{}, fmt.Errorf("%w: %s requires dynamicQuery.pagination with variable, cursorPath and hasNextPath",
			ErrRasterQuery, RasterTokensOwnedItemSchema)
	}
	u, err := url.Parse(dq.Endpoint)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return rasterWalkPlan{}, fmt.Errorf("%w: endpoint must be an absolute URL", ErrRasterQuery)
	}
	if u.User != nil {
		return rasterWalkPlan{}, fmt.Errorf("%w: endpoint must not include user info", ErrRasterQuery)
	}
	if !d.debug {
		if u.Scheme != "https" {
			return rasterWalkPlan{}, fmt.Errorf("%w: endpoint must be https", ErrRasterQuery)
		}
		if !rasterHosts[strings.ToLower(u.Hostname())] {
			return rasterWalkPlan{}, fmt.Errorf("%w: endpoint host %q is not a Raster host", ErrRasterQuery, u.Hostname())
		}
	}
	return rasterWalkPlan{
		endpoint:   dq.Endpoint,
		query:      dq.Query,
		headers:    dq.Headers,
		itemsPath:  dq.ResponseMapping.ItemsPath,
		pagination: *pag,
	}, nil
}

// processRasterWalk resolves a Raster wallet walk into playlist items.
//
// Paging is by cursor: the first page sends `variables: {}`, each following
// page sends `variables: {<pagination.variable>: <cursor>}`, where the cursor
// is read at `cursorPath` and the continue flag at `hasNextPath`. The walk
// stops when the flag is false, the cursor is empty, or the cursor repeats;
// it also stops after the first page for a minimal resolve, and fails past
// rasterMaxPages.
//
// Order is the app's, not Raster's (see sortRasterItems). That makes the
// maxItems cap subtle: Raster pages newest first, so once maxItems items are
// in hand the walk may only stop when the page it just read ends strictly
// OLDER than the item at the cap. If the page ends inside the cap item's
// timestamp, the tie group continues on the next page and a member of it may
// sort above the cap item, so the walk reads on until the group is complete
// (bounded by rasterMaxPages like any walk). A minimal resolve deliberately
// does not: it is one request by contract, so a tie group cut by its page
// boundary is ordered within what that page holds.
//
// ACCEPTED DEPARTURE from processDynamicPlaylistSpec's fail-closed rule: a
// node the mapper cannot turn into a playable item (no contract, no media
// URL, an unsupported chain) is skipped and logged with its Raster token id,
// not fatal. Raster carries on the order of a hundred metadata-less tokens in
// our catalog, and failing the cast on one of them would blank a wallet of
// over a thousand. A failed PAGE, by contrast, is still fatal and yields no
// partial playlist: the skip is per node, never per page.
func (d *dp1) processRasterWalk(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
	plan, err := d.planRasterWalk(playlist)
	if err != nil {
		return nil, err
	}
	if d.rasterAPIKey == "" {
		d.logger.Warn("raster apiKey not configured; walking the wallet unauthenticated",
			zap.String("playlist_id", playlist.ID))
	}

	var items []rasterWalkItem
	skipped := 0
	cursor := ""
	seen := map[string]bool{}
	pages := 0
	for {
		pages++
		if pages > rasterMaxPages {
			return nil, fmt.Errorf("%w: walk exceeded %d pages", ErrRasterQuery, rasterMaxPages)
		}
		root, err := d.fetchRasterPage(ctx, plan, cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %w", ErrRasterQuery, pages, err)
		}
		nodes, err := rasterNodesAt(root, plan.itemsPath)
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %w", ErrRasterQuery, pages, err)
		}
		for _, node := range nodes {
			item, reason := rasterNodeToWalkItem(node)
			if reason != "" {
				skipped++
				d.logger.Warn("raster node skipped",
					zap.String("playlist_id", playlist.ID),
					zap.String("raster_token_id", rasterTokenIDForLog(node)),
					zap.String("reason", reason))
				continue
			}
			items = append(items, item)
		}
		if minimal {
			break
		}
		if maxItems > 0 && len(items) >= maxItems && len(nodes) > 0 &&
			rasterAcquiredUs(nodes[len(nodes)-1].LastAcquiredAt) < items[maxItems-1].acquiredUs {
			// Everything still unread is older than the cap item; it cannot
			// make the cut. Skipped nodes count here too: they carry the
			// timestamp, and the page's last node marks how far the page got.
			break
		}
		hasNext, err := rasterBoolAt(root, plan.pagination.HasNextPath)
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %w", ErrRasterQuery, pages, err)
		}
		if !hasNext {
			break
		}
		next, err := rasterStringAt(root, plan.pagination.CursorPath)
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %w", ErrRasterQuery, pages, err)
		}
		if next == "" {
			break
		}
		if next == cursor || seen[next] {
			d.logger.Warn("raster cursor repeated; stopping the walk",
				zap.String("playlist_id", playlist.ID), zap.Int("pages", pages))
			break
		}
		seen[next] = true
		cursor = next
	}

	sortRasterItems(items)
	limit := len(items)
	if maxItems > 0 && limit > maxItems {
		limit = maxItems
	}
	if minimal && limit > MINIMAL_PLAYLIST_ITEMS_LIMIT {
		limit = MINIMAL_PLAYLIST_ITEMS_LIMIT
	}
	resolved := make([]dp1playlist.PlaylistItem, 0, limit)
	for _, walkItem := range items[:limit] {
		resolved = append(resolved, walkItem.item)
	}
	d.logger.Info("raster wallet walk resolved",
		zap.String("playlist_id", playlist.ID),
		zap.Int("pages", pages),
		zap.Int("items", len(resolved)),
		zap.Int("skipped", skipped))

	// Same replacement rule as the other dynamic paths: resolved items replace
	// whatever static items the document carried.
	playlist.Items = resolved
	return &playlist, nil
}

// fetchRasterPage posts one page request and returns the decoded envelope.
// A non-2xx status, an oversized or undecodable body, or a GraphQL `errors`
// array fails the page. Error text names the status, never the body, which
// could be large and is not ours.
func (d *dp1) fetchRasterPage(ctx context.Context, plan rasterWalkPlan, cursor string) (any, error) {
	variables := map[string]string{}
	if cursor != "" {
		variables[plan.pagination.Variable] = cursor
	}
	payload, err := json.Marshal(struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}{Query: plan.query, Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, plan.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range plan.headers {
		// The document's headers ride along as dp1-go would send them, except
		// the key header: the key is the device's, never the document's.
		if strings.EqualFold(k, rasterAPIKeyHeader) {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if d.rasterAPIKey != "" {
		req.Header.Set(rasterAPIKeyHeader, d.rasterAPIKey)
	}

	resp, err := d.rasterHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// rasterHTTP does not follow redirects, so the response's request is the
	// one built above. If it is not, the injected client followed one and the
	// key may already have left the host: fail loudly rather than carry on.
	if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != plan.endpoint {
		return nil, fmt.Errorf("request was redirected to %q; the Raster client must not follow redirects", resp.Request.URL.Host)
	}
	body, err := goio.ReadAll(goio.LimitReader(resp.Body, rasterMaxPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > rasterMaxPageBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", rasterMaxPageBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	// UseNumber keeps a numeric `tokenId` as its digits: through `any` a
	// number is a float64, which rounds anything past 2^53 and prints large
	// values in scientific notation, and the id feeds both provenance and the
	// item UUID. The nodes are re-encoded from this tree in rasterNodesAt,
	// where json.Number marshals back as the literal it was read from.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if msg, failed := graphqlErrorMessage(root); failed {
		return nil, fmt.Errorf("graphql: %s", msg)
	}
	return root, nil
}

// graphqlErrorMessage reports a non-empty top-level `errors` array, as dp1-go
// does: field-level errors beside a `data` object still fail the page, because
// a page with holes cannot be told from a complete one.
func graphqlErrorMessage(root any) (string, bool) {
	env, ok := root.(map[string]any)
	if !ok {
		return "", false
	}
	errs, ok := env["errors"].([]any)
	if !ok || len(errs) == 0 {
		return "", false
	}
	if first, ok := errs[0].(map[string]any); ok {
		if msg, ok := first["message"].(string); ok && msg != "" {
			return msg, true
		}
	}
	return "graphql error", true
}

// jsonAtDotPath walks `a.b.c` through nested objects. A missing key or a
// non-object on the way is an error naming the path: a page that lacks the
// documented fields is a failed page, never an empty one.
func jsonAtDotPath(root any, path string) (any, error) {
	cur := root
	for part := range strings.SplitSeq(path, ".") {
		if part == "" {
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path %q: %q is not inside an object", path, part)
		}
		cur, ok = obj[part]
		if !ok {
			return nil, fmt.Errorf("path %q: missing %q", path, part)
		}
	}
	return cur, nil
}

func rasterNodesAt(root any, path string) ([]rasterOwnedNode, error) {
	at, err := jsonAtDotPath(root, path)
	if err != nil {
		return nil, err
	}
	if at == nil {
		// A null connection body is an empty page, not a malformed one.
		return nil, nil
	}
	arr, ok := at.([]any)
	if !ok {
		return nil, fmt.Errorf("itemsPath %q is not an array", path)
	}
	out := make([]rasterOwnedNode, 0, len(arr))
	for i, el := range arr {
		// Round-trip through JSON rather than walking the generic tree by hand:
		// one decode rule for the whole node shape, nulls included.
		raw, err := json.Marshal(el)
		if err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
		var node rasterOwnedNode
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
		out = append(out, node)
	}
	return out, nil
}

func rasterBoolAt(root any, path string) (bool, error) {
	at, err := jsonAtDotPath(root, path)
	if err != nil {
		return false, err
	}
	b, ok := at.(bool)
	if !ok {
		return false, fmt.Errorf("path %q is not a boolean", path)
	}
	return b, nil
}

// rasterStringAt reads the cursor; JSON null (Raster's endCursor on an empty
// connection) reads as "".
func rasterStringAt(root any, path string) (string, error) {
	at, err := jsonAtDotPath(root, path)
	if err != nil {
		return "", err
	}
	if at == nil {
		return "", nil
	}
	s, ok := at.(string)
	if !ok {
		return "", fmt.Errorf("path %q is not a string", path)
	}
	return s, nil
}

func rasterTokenIDForLog(node rasterOwnedNode) string {
	if node.Token == nil {
		return ""
	}
	return rasterScalar(node.Token.ID)
}
