package control

import "testing"

func TestWindow(t *testing.T) {
	type step struct {
		ctr uint64
		ok  bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"zero refused", []step{{0, false}}},
		{"in order", []step{{1, true}, {2, true}, {3, true}}},
		{"duplicate", []step{{5, true}, {5, false}}},
		{"reorder inside window", []step{{10, true}, {7, true}, {9, true}, {7, false}, {8, true}}},
		{"edge of window", []step{{100, true}, {37, true}, {36, false}}},
		{"jump clears", []step{{1, true}, {200, true}, {1, false}, {137, true}, {136, false}}},
		{"below 64 at start", []step{{3, true}, {1, true}, {2, true}, {1, false}}},
		{"shift 63", []step{{1, true}, {64, true}, {1, false}, {2, true}}},
	}
	for _, tc := range cases {
		var w Window
		for i, s := range tc.steps {
			err := w.Check(s.ctr)
			if (err == nil) != s.ok {
				t.Fatalf("%s step %d ctr %d: err=%v want ok=%v (w=%+v)", tc.name, i, s.ctr, err, s.ok, w)
			}
			if err == nil {
				w = w.Accept(s.ctr)
				if !w.valid() {
					t.Fatalf("%s: invalid window %+v", tc.name, w)
				}
			}
		}
	}
}

func TestWindowValidAndHex(t *testing.T) {
	for _, tc := range []struct {
		w  Window
		ok bool
	}{
		{Window{}, true},
		{Window{Hi: 0, Bitmap: 1}, false},
		{Window{Hi: 5, Bitmap: 0}, false},
		{Window{Hi: 5, Bitmap: 0b11001}, true},
		{Window{Hi: 2, Bitmap: 0b101}, false}, // bit for ctr 0
		{Window{Hi: 100, Bitmap: ^uint64(0)}, true},
	} {
		if tc.w.valid() != tc.ok {
			t.Errorf("%+v valid=%v", tc.w, !tc.ok)
		}
	}
	w := Window{Hi: 70, Bitmap: 0x8000000000000001}
	h := w.bitmapHex()
	if h != "8000000000000001" {
		t.Fatal(h)
	}
	if v, err := parseBitmapHex(h); err != nil || v != w.Bitmap {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "1", "800000000000000G", "8000000000000001A", "ABCDEF0000000001"} {
		if _, err := parseBitmapHex(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
