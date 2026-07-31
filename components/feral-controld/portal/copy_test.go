package portal

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestTemplatesUseApprovedTerms is the user-facing copy check Canon's
// reference/voice/product-copy.md has each product repo carry (ff-app and
// ff-cli have equivalents; extend the word lists together). It scans the
// text a person actually sees — template HTML with comments, <style>,
// <script>, and {{...}} actions stripped — so identifiers where these
// tokens are legitimate (the ff1.config portal address, the FF1-<id> SSID
// arriving via {{.APSSID}}, CSS class names) never trip it.
//
// "frame" -> "Art Computer" is a repo-local rule beyond the Canon table:
// in portal copy "frame" always means the product.
func TestTemplatesUseApprovedTerms(t *testing.T) {
	strip := []*regexp.Regexp{
		regexp.MustCompile(`(?s)<!--.*?-->`),
		regexp.MustCompile(`(?s)<style>.*?</style>`),
		regexp.MustCompile(`(?s)<script>.*?</script>`),
		regexp.MustCompile(`{{.*?}}`),
	}

	banned := []struct {
		re  *regexp.Regexp
		fix string
	}{
		{regexp.MustCompile(`(?i)\bwifi\b`), "Wi-Fi"}, // \b splits the correct Wi-Fi at the hyphen
		{regexp.MustCompile(`\bDP1\b`), "DP-1"},
		{regexp.MustCompile(`\bFF1\b`), "Art Computer"}, // case-sensitive: ff1.config stays legal
		{regexp.MustCompile(`\bFFP\b`), "Art Panel"},
		{regexp.MustCompile(`(?i)\bferalfile\b`), "Feral File"},
		{regexp.MustCompile(`Device (?:Id|id)\b`), "Device ID"},
		{regexp.MustCompile(`(?i)\bframes?\b`), "Art Computer"},
	}

	entries, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no templates found; the copy check scanned nothing")
	}

	for _, name := range entries {
		raw, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		visible := string(raw)
		for _, re := range strip {
			visible = re.ReplaceAllString(visible, " ")
		}
		for lineNo, line := range strings.Split(visible, "\n") {
			for _, b := range banned {
				if hit := b.re.FindString(line); hit != "" {
					t.Errorf("%s:%d: %q -> use %q (see Canon reference/voice/product-copy.md)\n  line: %s",
						name, lineNo+1, hit, b.fix, strings.TrimSpace(line))
				}
			}
		}
	}
}
