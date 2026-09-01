package devicename_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/devicename"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "ordinary name is unchanged", in: "Living Room", want: "Living Room"},
		{name: "trims and collapses whitespace", in: "  Living   Room  ", want: "Living Room"},
		{name: "strips control characters", in: "Living\nRoom\x00", want: "Living Room"},
		{
			// A name set over the LAN command surface can carry these
			// invisibly; they would let one unit's record render as another's.
			name: "strips bidi overrides and zero-width",
			in:   "Living\u202eRoom\u200b",
			want: "LivingRoom",
		},
		{
			// The plain bidi marks set direction with no paired scope, so a
			// single invisible character reorders the label. Blocking the
			// override class without these would leave the easier attack open.
			name: "strips the bidi marks LRM, RLM and ALM",
			in:   "Living\u200eRoom\u200f\u061c",
			want: "LivingRoom",
		},
		{
			// Empty is a value, not a rejection: it is how an owner undoes a
			// rename, and the unit falls back to its serial.
			name: "input that cleans away to nothing yields empty",
			in:   "  \n\x00 ",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := devicename.Sanitize(tc.in); got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeTruncatesHard(t *testing.T) {
	// No ellipsis: the stored string is read back into the field the owner
	// edits next, so a decoration would become part of the name on the
	// following save.
	got := devicename.Sanitize(strings.Repeat("A", 100))

	if want := strings.Repeat("A", devicename.MaxLength); got != want {
		t.Fatalf("Sanitize(long) = %q, want %q", got, want)
	}
	if strings.Contains(got, "…") {
		t.Fatalf("Sanitize(long) = %q, want no ellipsis", got)
	}
}

func TestSanitizeCountsRunesNotBytes(t *testing.T) {
	// The limit exists for display slots, which are measured in characters.
	// Slicing bytes would also cut a multi-byte rune in half and emit
	// invalid UTF-8 into the mDNS record.
	in := strings.Repeat("é", 40)

	got := devicename.Sanitize(in)

	if n := len([]rune(got)); n != devicename.MaxLength {
		t.Fatalf("Sanitize(accented) kept %d runes, want %d", n, devicename.MaxLength)
	}
	if got != strings.Repeat("é", devicename.MaxLength) {
		t.Fatalf("Sanitize(accented) = %q, want %d accented runes", got, devicename.MaxLength)
	}
}

func TestSanitizeDropsTrailingSpaceFromTruncation(t *testing.T) {
	in := strings.Repeat("A", devicename.MaxLength) + " tail"

	if got, want := devicename.Sanitize(in), strings.Repeat("A", devicename.MaxLength); got != want {
		t.Fatalf("Sanitize(boundary) = %q, want %q", got, want)
	}
}

// fakeOS is an in-memory filesystem covering exactly the calls Save, Load and
// Clear make. The embedded interface leaves every other wrapper.OS method as a
// nil-panic, which is the point: a new OS call from this package should fail
// loudly here rather than silently touch the real disk.
type fakeOS struct {
	wrapper.OS
	files map[string][]byte
	dirs  map[string]bool
}

func newFakeOS() *fakeOS {
	return &fakeOS{files: map[string][]byte{}, dirs: map[string]bool{}}
}

var errFakeNotExist = errors.New("fake os: not exist")

func (f *fakeOS) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, errFakeNotExist
	}
	return data, nil
}

func (f *fakeOS) WriteFile(path string, data []byte, _ os.FileMode) error {
	f.files[path] = data
	return nil
}

func (f *fakeOS) MkdirAll(path string, _ os.FileMode) error {
	f.dirs[path] = true
	return nil
}

func (f *fakeOS) Rename(oldpath, newpath string) error {
	data, ok := f.files[oldpath]
	if !ok {
		return errFakeNotExist
	}
	f.files[newpath] = data
	delete(f.files, oldpath)
	return nil
}

func (f *fakeOS) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return errFakeNotExist
	}
	delete(f.files, path)
	return nil
}

func (f *fakeOS) IsNotExist(err error) bool { return errors.Is(err, errFakeNotExist) }

// TestSaveLoadClearRoundTrip pins the record lifecycle end to end: Save
// sanitizes before writing and stages through temp+rename, Load returns the
// stored form, and Clear removes the record.
func TestSaveLoadClearRoundTrip(t *testing.T) {
	osw, jsonw := newFakeOS(), wrapper.NewJSON()

	// Missing file is the ordinary unnamed state, not an error.
	rec, err := devicename.Load(osw, jsonw)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if rec.Name != "" {
		t.Fatalf("Load on missing file returned %q, want empty", rec.Name)
	}

	// Save sanitizes on the way in (zero-width space stripped, whitespace
	// collapsed); Load hands back the stored form.
	if err := devicename.Save(osw, jsonw, &devicename.Record{Name: "  Living\u200b   Room "}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, staged := osw.files[constants.DEVICE_NAME_FILE+".tmp"]; staged {
		t.Fatal("Save left its temp file behind after the rename")
	}
	rec, err = devicename.Load(osw, jsonw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Name != "Living Room" {
		t.Fatalf("Load returned %q, want the sanitized stored form", rec.Name)
	}

	// Clear removes the record; the unit is back to the unnamed state.
	if err := devicename.Clear(osw); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	rec, err = devicename.Load(osw, jsonw)
	if err != nil || rec.Name != "" {
		t.Fatalf("Load after Clear = (%q, %v), want empty and nil", rec.Name, err)
	}
	// Idempotent: clearing an already-clear unit is not an error.
	if err := devicename.Clear(osw); err != nil {
		t.Fatalf("Clear on empty state: %v", err)
	}
}

// TestClearRemovesStrandedTemp is the resold-frame regression: a crash between
// Save's WriteFile and Rename strands the previous owner's label at the .tmp
// path, and a factory reset that removed only the live record would hand the
// unit on with that label still on disk.
func TestClearRemovesStrandedTemp(t *testing.T) {
	osw := newFakeOS()
	osw.files[constants.DEVICE_NAME_FILE+".tmp"] = []byte(`{"name":"Sam's Studio"}`)
	osw.files[constants.DEVICE_NAME_FILE] = []byte(`{"name":"Sam's Studio"}`)

	if err := devicename.Clear(osw); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(osw.files) != 0 {
		t.Fatalf("Clear left files behind: %v", osw.files)
	}
}

// TestLoadCorruptRecordFailsSoft: a corrupt record is an error the caller
// logs, never a crash — every consumer falls back to the serial.
func TestLoadCorruptRecordFailsSoft(t *testing.T) {
	osw, jsonw := newFakeOS(), wrapper.NewJSON()
	osw.files[constants.DEVICE_NAME_FILE] = []byte(`{not json`)
	if _, err := devicename.Load(osw, jsonw); err == nil {
		t.Fatal("Load on corrupt record must return an error for the caller to log")
	}
}
