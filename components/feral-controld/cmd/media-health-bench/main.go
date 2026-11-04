// Command media-health-bench measures the cast-time L0 pass and the store
// against a real playlist (feral-file/ffos-user#392 acceptance: "a playlist
// of 1,000 works is measured: L0 latency added per item, storage used").
//
// It runs the same mediahealth.Prober the daemon runs, over the same guarded
// fetcher, against a temporary store, and prints: pass wall-clock, per-item
// latency, verdict counts by category, the hint payload size, and the store
// file size. Run it on the device or anywhere with the wall's network view:
//
//	go run ./cmd/media-health-bench -playlist https://feed.example/wall.json
//	go run ./cmd/media-health-bench -playlist ./wall.json -repeat 2
//
// -repeat runs the pass again to show the conclusive-store shortcut (the
// second pass should make no requests for items the first concluded).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

func main() {
	playlistArg := flag.String("playlist", "", "DP-1 playlist: a file path or an http(s) URL")
	repeat := flag.Int("repeat", 1, "number of passes (the second pass shows the stored-verdict shortcut)")
	storeDir := flag.String("store", "", "store directory (default: a temporary one)")
	flag.Parse()
	if *playlistArg == "" {
		flag.Usage()
		os.Exit(2)
	}

	items, err := loadPlaylist(*playlistArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load playlist:", err)
		os.Exit(1)
	}
	sources := make([]string, len(items))
	for i := range items {
		sources[i] = items[i].Source
	}

	dir := *storeDir
	if dir == "" {
		dir, err = os.MkdirTemp("", "media-health-bench-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer func() { _ = os.RemoveAll(dir) }()
	}
	storePath := filepath.Join(dir, "store.db")
	store, err := mediahealth.Open(storePath, mediahealth.Options{Logger: zap.NewNop()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	prober := &mediahealth.Prober{
		Probe: &mediahealth.Probe{
			Fetcher:   offlinecache.NewMediaHealthFetcher(net.DefaultResolver),
			Validator: mediahealth.NewContentValidator(mediahealth.DefaultProbeMaxBytes, nil),
		},
		Store: store,
	}

	fmt.Printf("items: %d (unique sources: %d)\n", len(items), uniqueCount(sources))
	for pass := 1; pass <= *repeat; pass++ {
		start := time.Now()
		results := prober.Preflight(context.Background(), sources)
		elapsed := time.Since(start)
		report(pass, elapsed, results)
	}

	hints, err := store.Hints(sources, time.Now())
	if err == nil {
		raw, _ := json.Marshal(hints)
		fmt.Printf("hint payload: %d broken items, %d bytes\n", len(hints), len(raw))
	}
	if info, err := os.Stat(storePath); err == nil {
		n, _ := store.Count()
		fmt.Printf("store: %d records, %d bytes on disk (%.0f B/record)\n", n, info.Size(), float64(info.Size())/float64(max(n, 1)))
	}
}

func report(pass int, elapsed time.Duration, results []mediahealth.ProbeResult) {
	counts := map[string]int{}
	fromStore := 0
	for _, r := range results {
		key := string(r.Verdict)
		if r.Verdict == mediahealth.VerdictDead {
			key += "/" + string(mediahealth.CategoryOf(r.Reason, r.Status))
		}
		counts[key]++
		if r.FromStore {
			fromStore++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("pass %d: %s total, %.1f ms/item, %d answered from the store\n",
		pass, elapsed.Round(time.Millisecond), float64(elapsed.Milliseconds())/float64(max(len(results), 1)), fromStore)
	for _, k := range keys {
		fmt.Printf("  %-32s %d\n", k, counts[k])
	}
}

func loadPlaylist(ref string) ([]dp1playlist.PlaylistItem, error) {
	var raw []byte
	var err error
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		resp, gerr := http.Get(ref) //nolint:gosec // G107: operator-supplied URL for a bench tool.
		if gerr != nil {
			return nil, gerr
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	} else {
		raw, err = os.ReadFile(ref) //nolint:gosec // G304: operator-supplied path for a bench tool.
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items []dp1playlist.PlaylistItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Items) == 0 {
		return nil, fmt.Errorf("playlist has no items")
	}
	return doc.Items, nil
}

func uniqueCount(sources []string) int {
	seen := map[string]struct{}{}
	for _, s := range sources {
		seen[s] = struct{}{}
	}
	return len(seen)
}
