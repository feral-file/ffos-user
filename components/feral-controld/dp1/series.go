package dp1

import (
	"context"
	"fmt"
	"net/url"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"
)

// A seriesRegistry item is a series, not an edition: core 1.1.0 §6's
// whole-series form, `contract.seriesId` with no `tokenId` (Sean,
// 2026-10-27). Its own `source` is one representative edition, so a player
// that knows nothing about series still shows the work.
//
// The editions come from the art catalog, which is the component that reads
// the series registry: `contract.seriesId` names an entry in the
// SeriesRegistry contract (feral-file/ff-evm-contracts), the contract holds
// that series' `tokenDataURI`, and the catalog resolves the two into items
// (Sang, 2026-10-28). This daemon asks the catalog rather than the chain
// index for three reasons:
//
//   - The index was keyed on the item's slug, as its `release_vendor_slug`.
//     A slug is editorial, not an identity: a series the index did not carry
//     under that slug had no editions at all, and nothing in DP-1 says a
//     slug means anything to a chain index.
//   - The catalog answers with DP-1 items that have already inherited from
//     the series item — duration, license, display, the manifest's artists —
//     so there is nothing to rebuild here. The index answered with tokens,
//     which had to be reshaped.
//   - One revision id per series comes back with them, so a client can tell
//     when the set moved without re-reading it.
//
// Every series is expanded, whatever playback mode the item asks for. The
// player narrows a full expansion back to the representative when the
// viewer's device setting says `first`, and narrowing needs no network —
// so the override is free in both directions, exactly as the device-level
// duration override is.
const (
	// The catalog this daemon reads editions from. A series item carries no
	// endpoint of its own (unlike a dynamicQuery, where it arrives inside
	// the signed document), so the default is named here and still passes
	// validateCatalogEndpoint.
	catalogBaseURL = "https://catalog.feralfile.io"

	// Editions fetched per request, and the ceiling on one series. A series
	// of 9,048 editions exists in the catalog; expanding it whole would
	// swamp a playlist whose other items are single works, so it is capped
	// at the same ceiling a dynamic playlist already respects. The catalog's
	// own page maximum is 500, so this asks for everything it will give in
	// one request.
	seriesPageLimit = MAX_PLAYLIST_ITEMS_LIMIT
)

// CATALOG_HOSTS is the allowlist validateCatalogEndpoint checks the base URL
// against, for the same reason FF_INDEXER_HOSTS exists: the answer decides
// what the screen plays, so the set of servers that may give it is closed.
// Both names front the same service and the same /api/v1 shape — production
// and the Development deployment the app also points at — so the one request
// buildCatalogEditionsURL builds is valid against either.
var CATALOG_HOSTS = map[string]bool{
	"catalog.feralfile.io":                                true,
	"art-catalog-service.bitmark-development.workers.dev": true,
}

// catalogEditions is the catalog's answer, of which only these two fields
// are read: `items` is the editions and `total` says how many exist, which
// is how a truncated expansion knows it was truncated.
type catalogEditions struct {
	Total int                        `json:"total"`
	Items []dp1playlist.PlaylistItem `json:"items"`
}

// expandSeriesItems replaces every seriesRegistry item in playlist.Items
// with the editions the catalog returns for it.
//
// An item the catalog cannot answer for is left exactly as it is: a series
// that fails to expand still plays its representative edition, which is the
// one outcome a screen must never lose. maxItems bounds the whole playlist,
// 0 meaning unbounded.
func (d *dp1) expandSeriesItems(ctx context.Context, playlist Playlist, maxItems int) Playlist {
	if !hasSeriesItems(playlist.Items) {
		return playlist
	}

	expanded := make([]dp1playlist.PlaylistItem, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		if !isSeriesItem(item) {
			expanded = append(expanded, item)
			continue
		}

		editions, err := d.seriesEditions(ctx, playlist.ID, item)
		if err != nil || len(editions) == 0 {
			if err != nil {
				d.logger.Warn("series item kept unexpanded; its representative still plays",
					zap.String("playlist_id", playlist.ID),
					zap.String("item_slug", item.Slug),
					zap.Error(err))
			}
			expanded = append(expanded, item)
			continue
		}

		d.logger.Info("expanded a series into its editions",
			zap.String("playlist_id", playlist.ID),
			zap.String("item_slug", item.Slug),
			zap.Int("editions", len(editions)))
		expanded = append(expanded, editions...)

		if maxItems > 0 && len(expanded) >= maxItems {
			expanded = expanded[:maxItems]
			break
		}
	}

	playlist.Items = expanded
	return playlist
}

// seriesEditions asks the catalog for one series' editions.
//
// The keys are the playlist's id and the item's, which is what the catalog
// addresses a series by — and they are the same ids wherever the playlist
// was fetched from, because ff3 derives both deterministically and the feed
// and the catalog carry them unchanged. A playlist the catalog does not hold
// (a cast of something never published, an older channel revision) answers
// 404, and the caller keeps the item.
//
// The result is incomplete for a Bitmark-era series whose editions never
// swapped to a contract: those have no token on chain, and reading them
// needs the Archive manifest registered on Ethereum (feral-file#3576 step
// 4). Until that lands, `full` plays the editions that are on chain.
func (d *dp1) seriesEditions(ctx context.Context, playlistID string, item dp1playlist.PlaylistItem) ([]dp1playlist.PlaylistItem, error) {
	if playlistID == "" || item.ID == "" {
		return nil, fmt.Errorf("series item has no playlist id and item id to resolve editions by")
	}

	endpoint, err := buildCatalogEditionsURL(catalogBaseURL, playlistID, item.ID, seriesPageLimit)
	if err != nil {
		return nil, err
	}

	resp, err := d.httpClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch series editions failed: %s", resp.Status)
	}

	body, err := d.io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var answer catalogEditions
	if err := d.json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}

	// The catalog has already merged each edition with its series item, so
	// its items go straight into the playlist. Only the cap is applied here:
	// one request asks for seriesPageLimit, and a series larger than that is
	// played in part rather than not at all.
	editions := answer.Items
	if len(editions) > seriesPageLimit {
		editions = editions[:seriesPageLimit]
	}
	if answer.Total > len(editions) {
		d.logger.Info("series expanded in part",
			zap.String("playlist_id", playlistID),
			zap.String("item_slug", item.Slug),
			zap.Int("editions", len(editions)),
			zap.Int("total", answer.Total))
	}
	return editions, nil
}

// buildCatalogEditionsURL is the catalog's editions route for one series
// item, validated before it is called.
func buildCatalogEditionsURL(base, playlistID, itemID string, limit int) (string, error) {
	if err := validateCatalogEndpoint(base); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/api/v1/playlists/%s/items/%s/editions?limit=%d",
		base, url.PathEscape(playlistID), url.PathEscape(itemID), limit), nil
}

// validateCatalogEndpoint validates that base is a catalog this daemon may
// read — see CATALOG_HOSTS for why the host set is closed. Matched on Host
// (so any port is rejected as a different host), never on a suffix: a suffix
// match would accept "catalog.feralfile.io.attacker.example". The scheme is
// checked as well, because the answer decides what the device displays and
// cleartext would let anyone on the path rewrite it.
func validateCatalogEndpoint(base string) error {
	parsed, err := url.Parse(base)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("invalid catalog scheme %q (https required): %s", parsed.Scheme, base)
	}
	if !CATALOG_HOSTS[parsed.Host] {
		return fmt.Errorf("invalid catalog endpoint: %s", base)
	}
	return nil
}

// isSeriesItem reports whether item names a series rather than one edition.
func isSeriesItem(item dp1playlist.PlaylistItem) bool {
	return item.Provenance != nil &&
		item.Provenance.Type == dp1playlist.ProvenanceSeriesRegistry
}

func hasSeriesItems(items []dp1playlist.PlaylistItem) bool {
	for _, item := range items {
		if isSeriesItem(item) {
			return true
		}
	}
	return false
}
