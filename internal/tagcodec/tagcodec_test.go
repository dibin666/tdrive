package tagcodec

import (
	"errors"
	"strings"
	"testing"
)

const (
	idA = "01K2QF3XR8ZZZZZZZZZZZZZZZZ"
	idB = "01K2QG7YM4YYYYYYYYYYYYYYYY"
	idC = "01K2QH9ABCXXXXXXXXXXXXXXXX"
)

func TestDirRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		dirName  string
		parentID string
		path     string
	}{
		{"ascii", "Movies", idB, "/Movies"},
		{"chinese", "电影", "", "/电影"},
		{"emoji", "🎬 片子", idB, "/🎬 片子"},
		{"spaces and dots", "  my. folder ", idB, "/  my. folder "},
		{"looks like a tag", "#tdrive", idB, "/#tdrive"},
		{"cyrillic", "Фильмы", "", "/Фильмы"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caption, err := EncodeDir(idA, tc.parentID, tc.dirName, tc.path)
			if err != nil {
				t.Fatalf("EncodeDir: %v", err)
			}
			got, err := Decode(caption)
			if err != nil {
				t.Fatalf("Decode(%q): %v", caption, err)
			}
			if got.Kind != KindDir {
				t.Errorf("Kind = %q, want %q", got.Kind, KindDir)
			}
			if got.ID != idA {
				t.Errorf("ID = %q, want %q", got.ID, idA)
			}
			if got.ParentID != tc.parentID {
				t.Errorf("ParentID = %q, want %q", got.ParentID, tc.parentID)
			}
			if got.Name != tc.dirName {
				t.Errorf("Name = %q, want %q", got.Name, tc.dirName)
			}
		})
	}
}

func TestFileRoundTrip(t *testing.T) {
	in := Record{
		Kind:        KindFile,
		ID:          idA,
		ParentID:    idB,
		Name:        "影片 [2024] 4K.mkv",
		SegIndex:    3,
		SegCount:    7,
		TotalSize:   13421772800,
		SegmentSize: 1992294400,
		HumanTags:   []string{"电影", "2024"},
	}

	caption, err := EncodeFile(in)
	if err != nil {
		t.Fatalf("EncodeFile: %v", err)
	}

	got, err := Decode(caption)
	if err != nil {
		t.Fatalf("Decode(%q): %v", caption, err)
	}
	if got.Name != in.Name {
		t.Errorf("Name = %q, want %q", got.Name, in.Name)
	}
	if got.SegIndex != 3 || got.SegCount != 7 {
		t.Errorf("segment = %d/%d, want 3/7", got.SegIndex, got.SegCount)
	}
	if got.TotalSize != in.TotalSize {
		t.Errorf("TotalSize = %d, want %d", got.TotalSize, in.TotalSize)
	}
	if got.SegmentSize != in.SegmentSize {
		t.Errorf("SegmentSize = %d, want %d", got.SegmentSize, in.SegmentSize)
	}

	// An all-digit folder name has to survive as a linkifiable tag.
	if !strings.Contains(caption, "#电影") || !strings.Contains(caption, "#_2024") {
		t.Errorf("human tags missing from caption:\n%s", caption)
	}
}

// A single-segment file must use the same shape as a split one so that the
// indexer and the reader have exactly one code path.
func TestSingleSegmentUsesSameShape(t *testing.T) {
	caption, err := EncodeFile(Record{
		ID: idA, ParentID: "", Name: "note.txt",
		SegIndex: 1, SegCount: 1, TotalSize: 12, SegmentSize: 1992294400,
	})
	if err != nil {
		t.Fatalf("EncodeFile: %v", err)
	}
	got, err := Decode(caption)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.SegIndex != 1 || got.SegCount != 1 {
		t.Errorf("segment = %d/%d, want 1/1", got.SegIndex, got.SegCount)
	}
	if got.ParentID != "" {
		t.Errorf("ParentID = %q, want root (empty)", got.ParentID)
	}
	if !strings.Contains(caption, "#pid_"+RootParent) {
		t.Errorf("root file should carry #pid_root:\n%s", caption)
	}
}

// Captions must fit Telegram's limit even at the worst case the VFS allows:
// a 255-byte name plus the deepest set of human tags.
func TestCaptionFitsTelegramLimit(t *testing.T) {
	longName := strings.Repeat("超长文件名", 16) // 240 bytes of UTF-8
	if len(longName) > MaxNameBytes {
		t.Fatalf("test fixture is %d bytes, over the %d limit", len(longName), MaxNameBytes)
	}
	caption, err := EncodeFile(Record{
		ID: idA, ParentID: idB, Name: longName + ".mkv",
		SegIndex: 12, SegCount: 99,
		TotalSize: 999999999999, SegmentSize: 1992294400,
		HumanTags: []string{
			strings.Repeat("目录", 40), strings.Repeat("另一个目录", 40),
			strings.Repeat("第三层", 40), strings.Repeat("第四层", 40),
			strings.Repeat("第五层", 40), strings.Repeat("第六层", 40),
		},
	})
	if err != nil {
		t.Fatalf("EncodeFile: %v", err)
	}
	if n := utf16Len(caption); n > MaxCaptionUnits {
		t.Fatalf("caption is %d UTF-16 units, limit is %d", n, MaxCaptionUnits)
	}

	// Trimming is only allowed to touch the cosmetic parts.
	got, err := Decode(caption)
	if err != nil {
		t.Fatalf("Decode after trimming: %v", err)
	}
	if got.Name != longName+".mkv" {
		t.Errorf("name did not survive trimming: got %q", got.Name)
	}
	if got.SegIndex != 12 || got.SegCount != 99 || got.TotalSize != 999999999999 {
		t.Errorf("segment metadata did not survive trimming: %+v", got)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := []struct {
		name    string
		caption string
		want    error
	}{
		{"empty", "", ErrNotTagged},
		{"unrelated message", "just a normal message\nwith #hashtags", ErrNotTagged},
		{"name that mentions the marker", "a file about #tdrive stuff", ErrNotTagged},
		{"no version", "x\n\n#tdrive #dir #id_" + idA, ErrMalformed},
		{"no kind", "x\n\n#tdrive #v1 #id_" + idA, ErrMalformed},
		{"bad id", "x\n\n#tdrive #v1 #dir #id_nope", ErrMalformed},
		{"bad parent id", "x\n\n#tdrive #v1 #dir #id_" + idA + " #pid_!!", ErrMalformed},
		{"future version", "x\n\n#tdrive #v99 #dir #id_" + idA, ErrUnknownVersion},
		{"file without segments", "x\n\n#tdrive #v1 #file #id_" + idA + " #n_MFYGC", ErrMalformed},
		{"segment index past count", "x\n\n#tdrive #v1 #file #id_" + idA + " #seg_9_3 #sz_1 #ss_1", ErrMalformed},
		{"zero segment count", "x\n\n#tdrive #v1 #file #id_" + idA + " #seg_0_0 #sz_1 #ss_1", ErrMalformed},
		{"non-numeric size", "x\n\n#tdrive #v1 #file #id_" + idA + " #seg_1_1 #sz_big #ss_1", ErrMalformed},
		{"undecodable name", "x\n\n#tdrive #v1 #dir #id_" + idA + " #n_1111", ErrMalformed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.caption)
			if !errors.Is(err, tc.want) {
				t.Errorf("Decode(%q) error = %v, want %v", tc.caption, err, tc.want)
			}
		})
	}
}

// A caption hand-edited in a Telegram client can lose #n_. Losing the exact
// name is better than dropping the file out of the drive.
func TestDecodeFallsBackToDisplayLine(t *testing.T) {
	got, err := Decode("影片.mkv\n\n#tdrive #v1 #file #id_" + idA + " #pid_" + idB + " #seg_2_5 #sz_100 #ss_20")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Name != "影片.mkv" {
		t.Errorf("Name = %q, want %q", got.Name, "影片.mkv")
	}

	dir, err := Decode("📁 /电影/2024\n\n#tdrive #v1 #dir #id_" + idA)
	if err != nil {
		t.Fatalf("Decode dir: %v", err)
	}
	if dir.Name != "2024" {
		t.Errorf("dir Name = %q, want %q", dir.Name, "2024")
	}
}

func TestEncodeRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
	}{
		{"empty name", Record{ID: idA, Name: "", SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 1}},
		{"name with slash", Record{ID: idA, Name: "a/b", SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 1}},
		{"name with newline", Record{ID: idA, Name: "a\nb", SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 1}},
		{"oversized name", Record{ID: idA, Name: strings.Repeat("x", MaxNameBytes+1), SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 1}},
		{"bad id", Record{ID: "short", Name: "a", SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 1}},
		{"segment out of range", Record{ID: idA, Name: "a", SegIndex: 4, SegCount: 3, TotalSize: 1, SegmentSize: 1}},
		{"zero segment size", Record{ID: idA, Name: "a", SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeFile(tc.rec); err == nil {
				t.Errorf("EncodeFile(%+v) succeeded, want error", tc.rec)
			}
		})
	}
}

func TestSanitizeTag(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Movies", "Movies"},
		{"电影", "电影"},
		{"My Folder", "MyFolder"},
		{"2024", "_2024"},
		{"v2024", "v2024"},
		{"...", ""},
		{"🎬", ""},
		{"a-b_c", "ab_c"},
		{"Ω", "Ω"},
	}
	for _, tc := range cases {
		if got := SanitizeTag(tc.in); got != tc.want {
			t.Errorf("SanitizeTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanTagsAreDedupedAndCapped(t *testing.T) {
	caption, err := EncodeFile(Record{
		ID: idA, ParentID: idB, Name: "f.bin",
		SegIndex: 1, SegCount: 1, TotalSize: 1, SegmentSize: 1,
		HumanTags: []string{"a", "a", "b", "...", "c", "d", "e", "f", "g"},
	})
	if err != nil {
		t.Fatalf("EncodeFile: %v", err)
	}
	rec, err := Decode(caption)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(rec.HumanTags) != MaxHumanTags {
		t.Errorf("got %d human tags (%v), want %d", len(rec.HumanTags), rec.HumanTags, MaxHumanTags)
	}
	if strings.Count(caption, "#a ")+strings.Count(caption, "#a\n") > 1 {
		t.Errorf("duplicate human tag survived:\n%s", caption)
	}
}

func TestDecodeNeverPanics(t *testing.T) {
	fuzzy := []string{
		"#tdrive",
		"#tdrive #v1",
		"#tdrive #v1 #file #seg_ #sz_ #ss_",
		"#tdrive #v #dir",
		"#tdrive #v1 #dir #id_" + idA + " #n_",
		"#tdrive\n#tdrive\n#tdrive",
		"#tdrive #v1 #dir #file #id_" + idA,
		strings.Repeat("#tdrive #v1 #dir ", 500),
		"\x00\xff\n#tdrive #v1 #dir #id_" + idA,
		"#tdrive #v1 #file #id_" + idA + " #seg_-1_-1 #sz_-5 #ss_-5",
		"#tdrive #v1 #file #id_" + idA + " #seg_1_1 #sz_99999999999999999999 #ss_1",
	}
	for _, c := range fuzzy {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Decode(%q) panicked: %v", c, r)
				}
			}()
			_, _ = Decode(c)
		}()
	}
}

func FuzzDecode(f *testing.F) {
	f.Add("影片.mkv\n\n#tdrive #v1 #file #id_" + idA + " #pid_" + idB + " #seg_1_2 #sz_10 #ss_5")
	f.Add("📁 /a\n\n#tdrive #v1 #dir #id_" + idC)
	f.Add("")
	f.Fuzz(func(t *testing.T, caption string) {
		rec, err := Decode(caption)
		if err != nil {
			return
		}
		// Anything Decode accepts must be internally consistent, because the
		// indexer trusts these fields when regrouping segments.
		if rec.Kind == KindFile && (rec.SegIndex < 1 || rec.SegIndex > rec.SegCount) {
			t.Fatalf("accepted inconsistent segment %d/%d from %q", rec.SegIndex, rec.SegCount, caption)
		}
		if rec.Name == "" {
			t.Fatalf("accepted empty name from %q", caption)
		}
		if err := validateID(rec.ID); err != nil {
			t.Fatalf("accepted bad id %q from %q", rec.ID, caption)
		}
	})
}
