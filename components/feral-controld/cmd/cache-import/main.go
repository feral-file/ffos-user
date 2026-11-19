// cache-import is an operator migration tool. It sends a verified observation
// pack through the controld-owned Store API; a pack is never an offline-ready
// assertion, and failed imports preserve the previous item record.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func run() error {
	if len(os.Args) != 4 {
		return fmt.Errorf("usage: cache-import CACHE-ROOT PACK-DIRECTORY ORIGINAL-SOURCE-URL")
	}
	store := offlinecache.NewStore(os.Args[1], wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	// The operator selects the pack root. Artist URLs never select local paths.
	pack, err := os.Open(filepath.Join(os.Args[2], "responses.json")) // #nosec G703 -- explicit operator filesystem argument
	if err != nil {
		return err
	}
	defer func() { _ = pack.Close() }()
	var entries map[string]struct {
		Status         int                        `json:"status"`
		Headers        []offlinecache.HeaderField `json:"headers"`
		SHA256         string                     `json:"sha256"`
		Size           int64                      `json:"size"`
		RequestHeaders map[string]string          `json:"requestHeaders"`
	}
	if stat, e := pack.Stat(); e != nil || stat.Size() > 16<<20 {
		return fmt.Errorf("pack index exceeds limit")
	}
	if err = json.NewDecoder(pack).Decode(&entries); err != nil {
		return err
	}
	if len(entries) > 4096 {
		return fmt.Errorf("pack entry limit")
	}
	record := &offlinecache.ItemRecord{Item: dp1playlist.PlaylistItem{Source: os.Args[3]}, Entry: os.Args[3], CapturedAt: time.Now().UTC(), Coverage: offlinecache.Coverage{Complete: false, Reason: "Imported observation; offline runtime acceptance pending"}}
	for key, res := range entries {
		method, url, ok := strings.Cut(key, " ")
		if !ok || method != "GET" || len(url) > 16384 || res.Size < 0 || res.Size > 32<<20 {
			return fmt.Errorf("unsupported pack request")
		}
		digest, e := hex.DecodeString(res.SHA256)
		if e != nil || len(digest) != 32 {
			return fmt.Errorf("invalid pack digest")
		}
		for name, value := range res.RequestHeaders {
			if value != "" && (strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization")) {
				return fmt.Errorf("pack contains session variant")
			}
		}
		var location, contentType string
		for _, h := range res.Headers {
			if strings.ContainsAny(h.Name, "\r\n:") || strings.ContainsAny(h.Value, "\r\n") {
				return fmt.Errorf("invalid pack header")
			}
			switch strings.ToLower(h.Name) {
			case "set-cookie":
				return fmt.Errorf("pack contains cookie response")
			case "vary":
				if !strings.EqualFold(strings.TrimSpace(h.Value), "accept-encoding") {
					return fmt.Errorf("unsupported pack Vary")
				}
			case "location":
				location = h.Value
			case "content-type":
				contentType = h.Value
			}
		}
		body, e := os.Open(filepath.Join(os.Args[2], "bodies", res.SHA256)) // #nosec G703 -- operator root; digest validated as exactly 32 hex bytes
		if e != nil {
			return e
		}
		stat, e := body.Stat()
		if e != nil || stat.Size() != res.Size {
			_ = body.Close()
			return fmt.Errorf("pack size mismatch")
		}
		sum, e := store.WriteBlob(body, 32<<20)
		_ = body.Close()
		if e != nil {
			return e
		}
		if sum != res.SHA256 {
			return fmt.Errorf("pack hash mismatch")
		}
		record.Resources = append(record.Resources, offlinecache.Resource{URL: url, Status: res.Status, SHA256: sum, ContentType: contentType, RedirectTo: location, BrowserPolicyVersion: 2, HeaderFields: res.Headers})
	}
	if err = store.SaveItem(record); err != nil {
		return err
	}
	fmt.Printf("Imported %d observed resources; coverage remains incomplete until verified\n", len(record.Resources))
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
