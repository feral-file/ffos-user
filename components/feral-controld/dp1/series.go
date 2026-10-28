package dp1

import (
	"context"
	"fmt"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
)

// A seriesRegistry item is a series, not an edition: core 1.1.0 §6's
// whole-series form, `contract.seriesId` with no `tokenId` (Sean,
// 2026-10-27). Its own `source` is one representative edition, so a player
// that knows nothing about series still shows the work.
//
// Expanding it into its editions is the same move the dynamicQuery path
// already makes — ask the chain index for tokens, build items from them —
// and it happens here for the same reason: the index endpoint is
// allowlisted in this daemon, and the player should receive items it can
// play rather than a query it has to run.
//
// Every series is expanded, whatever playback mode the item asks for. The
// player narrows a full expansion back to the representative when the
// viewer's device setting says `first`, and narrowing needs no network —
// so the override is free in both directions, exactly as the device-level
// duration override is.
const (
	// The chain index this daemon is allowed to reach. A series item carries
	// no endpoint of its own (unlike a dynamicQuery, where it arrives inside
	// the signed document), so the default is named here and still passes
	// validateEndpoint.
	seriesIndexEndpoint = "https://indexer-v2.feralfile.com/graphql"

	// Feral File is the only vendor whose series this daemon resolves today.
	seriesIndexVendor = "feralfile"

	// Editions fetched per request, and the ceiling on one series. A series
	// of 9,048 editions exists in the catalog; expanding it whole would
	// swamp a playlist whose other items are single works, so it is capped
	// at the same ceiling a dynamic playlist already respects.
	seriesPageLimit = MAX_PLAYLIST_ITEMS_LIMIT
)

// expandSeriesItems replaces every seriesRegistry item in playlist.Items
// with the editions the chain index returns for it.
//
// An item the index cannot answer for is left exactly as it is: a series
// that fails to expand still plays its representative edition, which is the
// one outcome a screen must never lose. maxItems bounds the whole playlist,
// 0 meaning unbounded.
func (d *dp1) expandSeriesItems(ctx context.Context, playlist Playlist, maxItems int) Playlist {
	if !hasSeriesItems(playlist.Items) {
		return playlist
	}

	duration := defaultDurationSeconds(playlist)
	expanded := make([]dp1playlist.PlaylistItem, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		if !isSeriesItem(item) {
			expanded = append(expanded, item)
			continue
		}

		editions, err := d.seriesEditions(ctx, item, duration)
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

// seriesEditions asks the chain index for one series' editions.
//
// The key is the item's slug, as the index's `release_vendor_slug`. That is
// a Feral File convention rather than anything DP-1 states: the index has no
// column for a contract address plus a series id, and Feral File's own
// release key is a UUID the playlist does not carry, so the slug is the only
// key an item and the index share today. A series whose slug the index does
// not know simply returns nothing, and the caller keeps the item.
//
// The result is incomplete for a Bitmark-era series whose editions never
// swapped to a contract: those have no token the index holds, and reading
// them needs the Archive manifest registered on Ethereum (feral-file#3576
// step 4). Until that lands, `full` plays the editions that are on chain.
func (d *dp1) seriesEditions(ctx context.Context, item dp1playlist.PlaylistItem, duration float64) ([]dp1playlist.PlaylistItem, error) {
	if item.Slug == "" {
		return nil, fmt.Errorf("series item has no slug to resolve editions by")
	}

	var tokens []ffindexer.Token
	offset := 0
	for {
		params := map[string]string{
			"release_vendor":      seriesIndexVendor,
			"release_vendor_slug": item.Slug,
			"limit":               fmt.Sprintf("%d", seriesPageLimit),
			"offset":              fmt.Sprintf("%d", offset),
		}
		page, err := d.ffIndexer.QueryTokens(ctx, seriesIndexEndpoint, params)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, page...)
		if len(page) < seriesPageLimit || len(tokens) >= seriesPageLimit {
			break
		}
		offset += seriesPageLimit
	}

	if len(tokens) > seriesPageLimit {
		tokens = tokens[:seriesPageLimit]
	}
	return buildPlaylistItems(duration, tokens), nil
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
