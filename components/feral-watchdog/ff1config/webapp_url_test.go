package ff1config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWebappURLFromPath_missingFile(t *testing.T) {
	t.Parallel()
	got := ResolveWebappURLFromPath(filepath.Join(t.TempDir(), "nope.json"))
	if got != DefaultWebappURL {
		t.Fatalf("got %q want %q", got, DefaultWebappURL)
	}
}

func TestResolveWebappURLFromPath_invalidJSON(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveWebappURLFromPath(p); got != DefaultWebappURL {
		t.Fatalf("got %q want %q", got, DefaultWebappURL)
	}
}

func TestResolveWebappURLFromPath_noField(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(`{"branch":"x","version":"1.0.0","endpoint":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveWebappURLFromPath(p); got != DefaultWebappURL {
		t.Fatalf("got %q want %q", got, DefaultWebappURL)
	}
}

func TestResolveWebappURLFromPath_explicitURL(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.json")
	raw := `{"branch":"x","version":"1.0.0","endpoint":"y","webapp_url":"http://127.0.0.1:8080/"}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8080/"
	if got := ResolveWebappURLFromPath(p); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveWebappURLFromPath_whitespaceTrimmed(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.json")
	raw := `{"webapp_url":"  https://example.test/path  "}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "https://example.test/path"
	if got := ResolveWebappURLFromPath(p); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveWebappURLFromPath_emptyStringFallsBack(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.json")
	raw := `{"webapp_url":""}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveWebappURLFromPath(p); got != DefaultWebappURL {
		t.Fatalf("got %q want %q", got, DefaultWebappURL)
	}
}

func TestResolveWebappURLFromPath_nullFallsBack(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "cfg.json")
	raw := `{"webapp_url":null}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveWebappURLFromPath(p); got != DefaultWebappURL {
		t.Fatalf("got %q want %q", got, DefaultWebappURL)
	}
}
