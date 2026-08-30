package database

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := db.CreateUser(ctx, "alice", "hash", RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	db.Close()

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()

	if _, err := db2.UserByName(ctx, "alice"); err != nil {
		t.Fatalf("user did not survive reopen: %v", err)
	}
}

func TestCacheUsageIncludesActiveStagedReservations(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	jobs := []DownloadJob{
		{ID: NewID(), Mode: DownloadStaged, Status: DownloadPending, TotalSize: 10},
		{ID: NewID(), Mode: DownloadStaged, Status: DownloadRunning, TotalSize: 20},
		{ID: NewID(), Mode: DownloadStaged, Status: DownloadReady, TotalSize: 30, CachePath: "/cache/ready"},
		{ID: NewID(), Mode: DownloadStaged, Status: DownloadFailed, TotalSize: 5, CachePath: "/cache/failed"},
		{ID: NewID(), Mode: DownloadDirect, Status: DownloadRunning, TotalSize: 40},
	}
	for _, job := range jobs {
		if err := db.InsertDownload(ctx, job); err != nil {
			t.Fatalf("InsertDownload: %v", err)
		}
	}

	used, count, err := db.CacheUsage(ctx)
	if err != nil {
		t.Fatalf("CacheUsage: %v", err)
	}
	if used != 65 || count != 4 {
		t.Fatalf("CacheUsage = (%d, %d), want (65, 4)", used, count)
	}
}

func TestUserUniquenessIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if _, err := db.CreateUser(ctx, "admin", "h", RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err := db.CreateUser(ctx, "Admin", "h", RoleUser)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("creating \"Admin\" returned %v, want ErrConflict", err)
	}

	// Login must find the account regardless of how it was typed.
	u, err := db.UserByName(ctx, "ADMIN")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	if u.Username != "admin" {
		t.Errorf("Username = %q, want %q", u.Username, "admin")
	}
}

func TestRefreshTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	u, err := db.CreateUser(ctx, "bob", "h", RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	hash := []byte("token-hash")
	tokenID, err := db.StoreRefreshToken(ctx, u.ID, hash, time.Now().Add(time.Hour), "", "")
	if err != nil {
		t.Fatalf("StoreRefreshToken: %v", err)
	}

	gotUser, gotToken, err := db.LookupRefreshToken(ctx, hash)
	if err != nil {
		t.Fatalf("LookupRefreshToken: %v", err)
	}
	if gotUser != u.ID || gotToken != tokenID {
		t.Errorf("lookup = (%q, %q), want (%q, %q)", gotUser, gotToken, u.ID, tokenID)
	}

	if err := db.RevokeRefreshToken(ctx, tokenID); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	if _, _, err := db.LookupRefreshToken(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token lookup returned %v, want ErrNotFound", err)
	}

	// An expired token must be rejected even though the row still exists.
	expired := []byte("expired-hash")
	if _, err := db.StoreRefreshToken(ctx, u.ID, expired, time.Now().Add(-time.Minute), "", ""); err != nil {
		t.Fatalf("StoreRefreshToken: %v", err)
	}
	if _, _, err := db.LookupRefreshToken(ctx, expired); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token lookup returned %v, want ErrNotFound", err)
	}
}

func mkdir(t *testing.T, db *DB, parentID, name, path string) Dir {
	t.Helper()
	d := Dir{ID: NewID(), ParentID: parentID, Name: name, Path: path}
	if err := db.InsertDir(context.Background(), d); err != nil {
		t.Fatalf("InsertDir %q: %v", path, err)
	}
	return d
}

// Renaming a directory has to rewrite every descendant path. SQLite's substr()
// counts characters while Go's len() counts bytes, so a tree with non-ASCII
// names is the case that actually exercises the offset arithmetic.
func TestRenameDirRewritesUnicodeDescendantPaths(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	top := mkdir(t, db, "", "电影收藏", "/电影收藏")
	mid := mkdir(t, db, top.ID, "2024年", "/电影收藏/2024年")
	mkdir(t, db, mid.ID, "4K原盘", "/电影收藏/2024年/4K原盘")

	// A sibling whose path shares the prefix but not the separator must not
	// be touched.
	sibling := mkdir(t, db, "", "电影收藏备份", "/电影收藏备份")

	if err := db.RenameDir(ctx, top.ID, "影片", "/影片"); err != nil {
		t.Fatalf("RenameDir: %v", err)
	}

	want := map[string]string{
		"/影片":            "影片",
		"/影片/2024年":      "2024年",
		"/影片/2024年/4K原盘": "4K原盘",
		"/电影收藏备份":        "电影收藏备份",
	}
	dirs, err := db.AllDirs(ctx)
	if err != nil {
		t.Fatalf("AllDirs: %v", err)
	}
	if len(dirs) != len(want) {
		t.Fatalf("got %d dirs, want %d", len(dirs), len(want))
	}
	for _, d := range dirs {
		name, ok := want[d.Path]
		if !ok {
			t.Errorf("unexpected path %q", d.Path)
			continue
		}
		if d.Name != name {
			t.Errorf("path %q has name %q, want %q", d.Path, d.Name, name)
		}
	}

	if got, err := db.DirByID(ctx, sibling.ID); err != nil || got.Path != "/电影收藏备份" {
		t.Errorf("sibling path = %q (err %v), want /电影收藏备份", got.Path, err)
	}
}

// A directory literally named with a LIKE wildcard must not drag its siblings
// along during a rename.
func TestRenameDirEscapesLikeWildcards(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	target := mkdir(t, db, "", "100%", "/100%")
	mkdir(t, db, target.ID, "inner", "/100%/inner")
	decoy := mkdir(t, db, "", "100XX", "/100XX")
	mkdir(t, db, decoy.ID, "inner", "/100XX/inner")

	if err := db.RenameDir(ctx, target.ID, "full", "/full"); err != nil {
		t.Fatalf("RenameDir: %v", err)
	}

	if d, err := db.DirByPath(ctx, "/full/inner"); err != nil {
		t.Errorf("target child not rewritten: %v", err)
	} else if d.Name != "inner" {
		t.Errorf("name = %q", d.Name)
	}
	if _, err := db.DirByPath(ctx, "/100XX/inner"); err != nil {
		t.Errorf("decoy subtree was rewritten: %v", err)
	}
}

func TestDirAndFileNameConflicts(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	parent := mkdir(t, db, "", "docs", "/docs")
	mkdir(t, db, parent.ID, "a", "/docs/a")

	err := db.InsertDir(ctx, Dir{ID: NewID(), ParentID: parent.ID, Name: "a", Path: "/docs/a-other"})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate dir name returned %v, want ErrConflict", err)
	}

	f := File{ID: NewID(), DirID: parent.ID, Name: "x.bin", Size: 10, SegmentSize: 100, SegmentCount: 1, Status: StatusComplete}
	if err := db.InsertFile(ctx, f); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	dup := File{ID: NewID(), DirID: parent.ID, Name: "x.bin", Size: 20, SegmentSize: 100, SegmentCount: 1, Status: StatusComplete}
	if err := db.InsertFile(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate file name returned %v, want ErrConflict", err)
	}

	// The same name at the root is a different namespace and must be allowed.
	root := File{ID: NewID(), Name: "x.bin", Size: 1, SegmentSize: 100, SegmentCount: 1, Status: StatusComplete}
	if err := db.InsertFile(ctx, root); err != nil {
		t.Errorf("same name at root rejected: %v", err)
	}
}

func TestDeleteDirCascades(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	top := mkdir(t, db, "", "top", "/top")
	sub := mkdir(t, db, top.ID, "sub", "/top/sub")

	f := File{ID: NewID(), DirID: sub.ID, Name: "big.mkv", Size: 300, SegmentSize: 100, SegmentCount: 3, Status: StatusComplete}
	if err := db.InsertFile(ctx, f); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := db.UpsertSegment(ctx, Segment{FileID: f.ID, Index: i, Size: 100, TGMsgID: 1000 + i}); err != nil {
			t.Fatalf("UpsertSegment: %v", err)
		}
	}

	msgs, err := db.SubtreeMessages(ctx, top.ID)
	if err != nil {
		t.Fatalf("SubtreeMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("got %d messages to delete, want 3 (the segments)", len(msgs))
	}

	if err := db.DeleteDir(ctx, top.ID); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	if _, err := db.FileByID(ctx, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("file survived cascade: %v", err)
	}
	segs, err := db.Segments(ctx, f.ID)
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("%d segments survived cascade", len(segs))
	}
}

func TestPendingFilesAreHidden(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	dir := mkdir(t, db, "", "d", "/d")
	pending := File{ID: NewID(), DirID: dir.ID, Name: "wip.bin", Size: 1, SegmentSize: 1, SegmentCount: 1, Status: StatusPending}
	done := File{ID: NewID(), DirID: dir.ID, Name: "ok.bin", Size: 1, SegmentSize: 1, SegmentCount: 1, Status: StatusComplete}
	broken := File{ID: NewID(), DirID: dir.ID, Name: "bad.bin", Size: 1, SegmentSize: 1, SegmentCount: 2, Status: StatusBroken}
	for _, f := range []File{pending, done, broken} {
		if err := db.InsertFile(ctx, f); err != nil {
			t.Fatalf("InsertFile %q: %v", f.Name, err)
		}
	}

	files, err := db.ListFiles(ctx, dir.ID)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 (complete and broken, not pending)", len(files))
	}
	for _, f := range files {
		if f.Status == StatusPending {
			t.Errorf("pending file %q was listed", f.Name)
		}
	}
}

func TestMaskBitset(t *testing.T) {
	m := NewMask(10)
	if len(m) != 2 {
		t.Fatalf("NewMask(10) length = %d, want 2", len(m))
	}
	for i := 1; i <= 10; i++ {
		if MaskHas(m, i) {
			t.Fatalf("fresh mask has bit %d set", i)
		}
	}
	m = MaskSet(m, 1)
	m = MaskSet(m, 8)
	m = MaskSet(m, 9)
	m = MaskSet(m, 10)
	for _, want := range []struct {
		idx int
		set bool
	}{{1, true}, {2, false}, {7, false}, {8, true}, {9, true}, {10, true}, {11, false}, {0, false}, {-1, false}} {
		if got := MaskHas(m, want.idx); got != want.set {
			t.Errorf("MaskHas(%d) = %v, want %v", want.idx, got, want.set)
		}
	}

	// Growing past the allocated length must not panic or lose earlier bits.
	m = MaskSet(m, 40)
	if !MaskHas(m, 40) || !MaskHas(m, 1) {
		t.Errorf("growth lost bits: %v", m)
	}
}

// Segments of one file land concurrently. The bitset update has to be
// atomic or the job never reaches complete.
func TestMarkSegmentDoneIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	u, err := db.CreateUser(ctx, "u", "h", RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	const n = 16
	job := UploadJob{
		ID: NewID(), UserID: u.ID, Name: "big.mkv",
		TotalSize: n * 100, SegmentSize: 100, SegmentCount: n,
		DoneMask: NewMask(n), Status: JobRunning,
	}
	if err := db.InsertJob(ctx, job); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx-1] = db.MarkSegmentDone(ctx, job.ID, idx, 100)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("MarkSegmentDone(%d): %v", i+1, err)
		}
	}

	got, err := db.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if !got.Done() {
		t.Errorf("job not done, pending segments: %v", got.PendingSegments())
	}
	if got.UploadedBytes != n*100 {
		t.Errorf("UploadedBytes = %d, want %d", got.UploadedBytes, n*100)
	}
	if got.Status != JobComplete {
		t.Errorf("Status = %q, want %q", got.Status, JobComplete)
	}
}

// A retried segment must not be counted twice, or progress runs past 100%.
func TestMarkSegmentDoneIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	u, _ := db.CreateUser(ctx, "u", "h", RoleUser)
	job := UploadJob{
		ID: NewID(), UserID: u.ID, Name: "f", TotalSize: 200,
		SegmentSize: 100, SegmentCount: 2, DoneMask: NewMask(2), Status: JobRunning,
	}
	if err := db.InsertJob(ctx, job); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := db.MarkSegmentDone(ctx, job.ID, 1, 100); err != nil {
			t.Fatalf("MarkSegmentDone: %v", err)
		}
	}
	got, _ := db.JobByID(ctx, job.ID)
	if got.UploadedBytes != 100 {
		t.Errorf("UploadedBytes = %d after 3 identical reports, want 100", got.UploadedBytes)
	}
	if want := []int{2}; len(got.PendingSegments()) != 1 || got.PendingSegments()[0] != want[0] {
		t.Errorf("PendingSegments = %v, want %v", got.PendingSegments(), want)
	}
}

func TestDefaultChannelSwitch(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	a, err := db.UpsertChannel(ctx, -100123, 555, "Drive A")
	if err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	b, err := db.UpsertChannel(ctx, -100456, 666, "Drive B")
	if err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}

	if err := db.SetDefaultChannel(ctx, a.ID); err != nil {
		t.Fatalf("SetDefaultChannel: %v", err)
	}
	if err := db.SetDefaultChannel(ctx, b.ID); err != nil {
		t.Fatalf("SetDefaultChannel: %v", err)
	}

	def, err := db.DefaultChannel(ctx)
	if err != nil {
		t.Fatalf("DefaultChannel: %v", err)
	}
	if def.ID != b.ID {
		t.Errorf("default is %q, want %q", def.Title, b.Title)
	}

	// Re-selecting an existing channel refreshes its access hash rather than
	// inserting a duplicate; a stale hash breaks every download.
	again, err := db.UpsertChannel(ctx, -100123, 999, "Drive A renamed")
	if err != nil {
		t.Fatalf("UpsertChannel again: %v", err)
	}
	if again.ID != a.ID {
		t.Errorf("re-upsert created a new row %q, want %q", again.ID, a.ID)
	}
	if again.AccessHash != 999 || again.Title != "Drive A renamed" {
		t.Errorf("re-upsert did not refresh: %+v", again)
	}
	chans, _ := db.ListChannels(ctx)
	if len(chans) != 2 {
		t.Errorf("got %d channels, want 2", len(chans))
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if got := db.SettingOr(ctx, SettingTGAppHash, "fallback"); got != "fallback" {
		t.Errorf("missing setting = %q, want fallback", got)
	}
	if err := db.SetSetting(ctx, SettingTGAppHash, "abc"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := db.SetSetting(ctx, SettingTGAppHash, "def"); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}
	if got := db.SettingOr(ctx, SettingTGAppHash, ""); got != "def" {
		t.Errorf("setting = %q, want def", got)
	}
}
