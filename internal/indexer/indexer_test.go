package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/tagcodec"
)

// A rebuild has to reproduce the drive from captions alone. These tests build a
// channel by hand, wipe the index, and check that what comes back is what went
// in — including the awkward cases a damaged channel produces.

// channel is a list of messages standing in for a Telegram channel, newest
// first, which is the order Telegram returns history in.
type channel struct {
	messages []Message
}

func (c *channel) ScanHistory(_ context.Context, _ drive.ChannelRef, visit func(Message) error) error {
	for _, m := range c.messages {
		if err := visit(m); err != nil {
			return err
		}
	}
	return nil
}

// ID names the account doing the scan, which the rebuild stamps onto every
// recovered segment so a reader knows whose access hashes it recorded.
func (c *channel) ID() string { return scannerAccountID }

const scannerAccountID = "acct-scanner"

// builder assembles a channel in upload order and reverses it on Build, since
// history arrives newest first.
type builder struct {
	msgs    []Message
	nextID  int
	nextDoc int64
}

func newBuilder() *builder { return &builder{nextID: 1000, nextDoc: 500} }

func (b *builder) dir(t *testing.T, id, parentID, name, path string) {
	t.Helper()
	caption, err := tagcodec.EncodeDir(id, parentID, name, path)
	if err != nil {
		t.Fatalf("encode dir %q: %v", path, err)
	}
	b.nextID++
	b.msgs = append(b.msgs, Message{ID: b.nextID, Caption: caption})
}

// file writes one file as segCount segment messages, the way an upload would.
func (b *builder) file(t *testing.T, id, dirID, name string, total, segSize int64, segCount int, tags ...string) {
	t.Helper()
	for i := 1; i <= segCount; i++ {
		caption, err := tagcodec.EncodeFile(tagcodec.Record{
			Kind: tagcodec.KindFile, ID: id, ParentID: dirID, Name: name,
			SegIndex: i, SegCount: segCount, TotalSize: total, SegmentSize: segSize,
			HumanTags: tags,
		})
		if err != nil {
			t.Fatalf("encode file %q segment %d: %v", name, i, err)
		}
		b.nextID++
		b.nextDoc++
		size := min(segSize, total-int64(i-1)*segSize)
		b.msgs = append(b.msgs, Message{
			ID:      b.nextID,
			Caption: caption,
			Doc: &drive.StoredDoc{
				MsgID: b.nextID, DocID: b.nextDoc, AccessHash: b.nextDoc * 3,
				DCID: 2, FileReference: []byte("ref"), Size: size,
			},
		})
	}
}

// partial writes a file whose segments do not all exist, which is what a
// channel looks like after someone deleted a message by hand.
func (b *builder) partial(t *testing.T, id, dirID, name string, total, segSize int64, segCount int, present []int) {
	t.Helper()
	for _, i := range present {
		caption, err := tagcodec.EncodeFile(tagcodec.Record{
			Kind: tagcodec.KindFile, ID: id, ParentID: dirID, Name: name,
			SegIndex: i, SegCount: segCount, TotalSize: total, SegmentSize: segSize,
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		b.nextID++
		b.nextDoc++
		b.msgs = append(b.msgs, Message{
			ID:      b.nextID,
			Caption: caption,
			Doc:     &drive.StoredDoc{MsgID: b.nextID, DocID: b.nextDoc, Size: segSize},
		})
	}
}

func (b *builder) noise(text string) {
	b.nextID++
	b.msgs = append(b.msgs, Message{ID: b.nextID, Caption: text})
}

func (b *builder) build() *channel {
	// Telegram returns newest first.
	out := make([]Message, len(b.msgs))
	for i, m := range b.msgs {
		out[len(b.msgs)-1-i] = m
	}
	return &channel{messages: out}
}

func newIndexer(t *testing.T, src Source) (*Indexer, *database.DB) {
	t.Helper()
	ctx := context.Background()

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ch, err := db.UpsertChannel(ctx, -1001, 7, "drive")
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := db.SetDefaultChannel(ctx, ch.ID); err != nil {
		t.Fatalf("default channel: %v", err)
	}

	return New(db, src, zap.NewNop()), db
}

// runRebuild starts a rebuild and waits for it, since Start is asynchronous.
func runRebuild(t *testing.T, ix *Indexer) Progress {
	t.Helper()
	if err := ix.Start(context.Background()); err != nil {
		t.Fatalf("start rebuild: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := ix.Status()
		if status.Done {
			if status.Error != "" {
				t.Fatalf("rebuild failed: %s", status.Error)
			}
			return status
		}
		if time.Now().After(deadline) {
			t.Fatal("rebuild did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		// Valid Crockford ULIDs, ordered so the oldest sorts first.
		out[i] = fmt.Sprintf("01K2QF3XR8%016d", i)
	}
	return out
}

// The whole point: a tree with nested folders and a split file comes back
// intact from captions alone.
func TestRebuildRestoresTheTree(t *testing.T) {
	id := ids(6)
	b := newBuilder()

	b.dir(t, id[0], "", "电影", "/电影")
	b.dir(t, id[1], id[0], "2024", "/电影/2024")
	b.dir(t, id[2], "", "文档", "/文档")

	b.file(t, id[3], id[1], "影片 4K.mkv", 12_000, 4096, 3, "2024", "电影")
	b.file(t, id[4], id[2], "notes.txt", 500, 4096, 1, "文档")
	b.file(t, id[5], "", "root.bin", 8192, 4096, 2)

	// A human chatting in the storage channel must not break anything.
	b.noise("hey, is this the backup channel?")
	b.noise("#notatdrivetag just a hashtag")

	ix, db := newIndexer(t, b.build())
	got := runRebuild(t, ix)

	if got.Dirs != 3 {
		t.Errorf("rebuilt %d directories, want 3", got.Dirs)
	}
	if got.Files != 3 {
		t.Errorf("rebuilt %d files, want 3", got.Files)
	}
	if got.Segments != 6 {
		t.Errorf("rebuilt %d segments, want 6", got.Segments)
	}
	if got.Broken != 0 {
		t.Errorf("%d files came back broken", got.Broken)
	}

	ctx := context.Background()
	dirs, err := db.AllDirs(ctx)
	if err != nil {
		t.Fatalf("list dirs: %v", err)
	}
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = d.Path
	}
	sort.Strings(paths)
	want := []string{"/文档", "/电影", "/电影/2024"}
	sort.Strings(want)
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}

	// The split file must come back as one row with the right size and count.
	deep, err := db.DirByPath(ctx, "/电影/2024")
	if err != nil {
		t.Fatalf("resolve /电影/2024: %v", err)
	}
	files, err := db.ListFiles(ctx, deep.ID)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files in /电影/2024, want 1", len(files))
	}
	movie := files[0]
	if movie.Name != "影片 4K.mkv" {
		t.Errorf("name = %q", movie.Name)
	}
	if movie.Size != 12_000 {
		t.Errorf("size = %d, want 12000", movie.Size)
	}
	if movie.SegmentCount != 3 {
		t.Errorf("segment count = %d, want 3", movie.SegmentCount)
	}
	if movie.Status != database.StatusComplete {
		t.Errorf("status = %q, want complete", movie.Status)
	}

	segs, err := db.Segments(ctx, movie.ID)
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segment rows, want 3", len(segs))
	}
	for i, seg := range segs {
		if seg.Index != i+1 {
			t.Errorf("segment %d has index %d", i, seg.Index)
		}
		if seg.TGDocID == 0 {
			t.Errorf("segment %d lost its document id", seg.Index)
		}
	}
}

// A rebuild replaces whatever was there, so stale rows must not survive.
func TestRebuildReplacesStaleRows(t *testing.T) {
	id := ids(2)
	b := newBuilder()
	b.dir(t, id[0], "", "kept", "/kept")

	ix, db := newIndexer(t, b.build())
	ctx := context.Background()

	// Something the channel knows nothing about.
	stale := database.Dir{ID: id[1], Name: "ghost", Path: "/ghost"}
	if err := db.InsertDir(ctx, stale); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	runRebuild(t, ix)

	if _, err := db.DirByPath(ctx, "/ghost"); err == nil {
		t.Error("a directory absent from the channel survived the rebuild")
	}
	if _, err := db.DirByPath(ctx, "/kept"); err != nil {
		t.Errorf("a directory present in the channel was lost: %v", err)
	}
}

// A file missing segments must be surfaced, not hidden. Quietly dropping a
// partially recoverable multi-gigabyte file is the worse failure.
func TestRebuildFlagsIncompleteFiles(t *testing.T) {
	id := ids(2)
	b := newBuilder()
	b.dir(t, id[0], "", "media", "/media")
	b.partial(t, id[1], id[0], "torn.mkv", 20_000, 4096, 5, []int{1, 2, 5})

	ix, db := newIndexer(t, b.build())
	got := runRebuild(t, ix)

	if got.Broken != 1 {
		t.Errorf("reported %d broken files, want 1", got.Broken)
	}

	ctx := context.Background()
	dir, err := db.DirByPath(ctx, "/media")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	files, err := db.ListFiles(ctx, dir.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want the broken one to still be listed", len(files))
	}
	if files[0].Status != database.StatusBroken {
		t.Errorf("status = %q, want broken", files[0].Status)
	}
}

// A directory whose parent record was deleted must not take its subtree down
// with it.
func TestRebuildRescuesOrphanedSubtrees(t *testing.T) {
	id := ids(5)
	missingParent := "01K2QF3XR8ZZZZZZZZZZZZZZZZ"

	b := newBuilder()
	// This directory's parent is never written, simulating a deleted record.
	b.dir(t, id[0], missingParent, "orphan", "/gone/orphan")
	b.dir(t, id[1], id[0], "child", "/gone/orphan/child")
	b.dir(t, id[2], id[1], "grandchild", "/gone/orphan/child/grandchild")
	b.file(t, id[3], id[2], "deep.bin", 100, 4096, 1)
	b.dir(t, id[4], "", "normal", "/normal")

	ix, db := newIndexer(t, b.build())
	runRebuild(t, ix)

	ctx := context.Background()
	dirs, err := db.AllDirs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byPath := map[string]bool{}
	for _, d := range dirs {
		byPath[d.Path] = true
	}

	// The subtree keeps its shape, rooted under the recovery folder.
	for _, want := range []string{
		"/" + RecoveredDir,
		"/" + RecoveredDir + "/orphan",
		"/" + RecoveredDir + "/orphan/child",
		"/" + RecoveredDir + "/orphan/child/grandchild",
		"/normal",
	} {
		if !byPath[want] {
			t.Errorf("missing %q; got %v", want, keys(byPath))
		}
	}

	// The file at the bottom has to remain reachable, which is the whole point.
	deep, err := db.DirByPath(ctx, "/"+RecoveredDir+"/orphan/child/grandchild")
	if err != nil {
		t.Fatalf("resolve rescued dir: %v", err)
	}
	files, err := db.ListFiles(ctx, deep.ID)
	if err != nil || len(files) != 1 {
		t.Errorf("rescued file is missing: %d files, err %v", len(files), err)
	}
}

// Corrupted captions could describe a cycle. The rebuild must terminate and
// still produce a usable tree.
func TestRebuildBreaksCycles(t *testing.T) {
	id := ids(3)
	b := newBuilder()
	// a -> b -> a
	b.dir(t, id[0], id[1], "a", "/a")
	b.dir(t, id[1], id[0], "b", "/b")
	b.dir(t, id[2], "", "sane", "/sane")

	ix, db := newIndexer(t, b.build())

	done := make(chan Progress, 1)
	go func() { done <- runRebuild(t, ix) }()

	select {
	case got := <-done:
		if got.Dirs < 3 {
			t.Errorf("rebuilt %d directories, want at least the 3 records", got.Dirs)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a directory cycle made the rebuild hang")
	}

	if _, err := db.DirByPath(context.Background(), "/sane"); err != nil {
		t.Errorf("the unaffected directory was lost: %v", err)
	}
}

// Two rescued directories can want the same name. One has to be renamed, and
// its children have to follow it.
func TestRebuildDeduplicatesCollidingPaths(t *testing.T) {
	id := ids(4)
	gone1 := "01K2QF3XR8ZZZZZZZZZZZZZZZA"
	gone2 := "01K2QF3XR8ZZZZZZZZZZZZZZZB"

	b := newBuilder()
	b.dir(t, id[0], gone1, "shared", "/x/shared")
	b.dir(t, id[1], id[0], "inner", "/x/shared/inner")
	b.dir(t, id[2], gone2, "shared", "/y/shared")

	ix, db := newIndexer(t, b.build())
	runRebuild(t, ix)

	dirs, err := db.AllDirs(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	paths := map[string]int{}
	for _, d := range dirs {
		paths[d.Path]++
	}
	for path, n := range paths {
		if n > 1 {
			t.Errorf("path %q was written %d times", path, n)
		}
	}

	// The renamed directory's child must sit under the new name, not the old.
	var innerPath string
	for _, d := range dirs {
		if d.Name == "inner" {
			innerPath = d.Path
		}
	}
	if innerPath == "" {
		t.Fatal("the child directory was lost")
	}
	parent := innerPath[:strings.LastIndex(innerPath, "/")]
	if _, ok := paths[parent]; !ok {
		t.Errorf("child sits at %q but its parent %q does not exist", innerPath, parent)
	}
}

// The most recent version of an edited caption wins, since a rename rewrites
// the message in place and history returns the current text.
func TestRebuildUsesTheCurrentCaption(t *testing.T) {
	id := ids(1)
	b := newBuilder()
	b.dir(t, id[0], "", "renamed", "/renamed")

	ix, db := newIndexer(t, b.build())
	runRebuild(t, ix)

	if _, err := db.DirByPath(context.Background(), "/renamed"); err != nil {
		t.Errorf("expected the current name: %v", err)
	}
}

func TestRebuildRefusesToRunTwice(t *testing.T) {
	b := newBuilder()
	for i := range 200 {
		b.noise(fmt.Sprintf("message %d", i))
	}
	ix, _ := newIndexer(t, &slowChannel{inner: b.build(), delay: 20 * time.Millisecond})

	if err := ix.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := ix.Start(context.Background()); err == nil {
		t.Error("a second concurrent rebuild was allowed")
	}
	// Let it finish so the test does not leave a goroutine writing to the db.
	deadline := time.Now().Add(10 * time.Second)
	for !ix.Status().Done && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

type slowChannel struct {
	inner *channel
	delay time.Duration
}

func (s *slowChannel) ScanHistory(ctx context.Context, ch drive.ChannelRef, visit func(Message) error) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.inner.ScanHistory(ctx, ch, visit)
}

func (s *slowChannel) ID() string { return s.inner.ID() }

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
