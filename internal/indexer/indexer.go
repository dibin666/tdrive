// Package indexer rebuilds the SQLite index by replaying a Telegram channel.
//
// This is the payoff of putting structured tags in every caption. The database
// is a cache: lose it, corrupt it, or move the drive to a new machine, and the
// whole tree — directory hierarchy, file names, which documents are segments of
// which file and in what order — comes back from the channel itself.
//
// It is also the check that keeps the design honest. If a rebuild cannot
// reproduce the drive, the captions were not carrying enough, and that is a bug
// in the writer rather than in this package.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/tagcodec"
)

// Message is one record read back from a channel. Doc is nil for the
// caption-only messages that represent directories and empty files.
type Message struct {
	ID      int
	Caption string
	Doc     *drive.StoredDoc
}

// Source supplies a channel's history, newest message first.
//
// It is an interface so a rebuild can be tested without a Telegram connection —
// which matters because this is the disaster-recovery path, and a bug here only
// shows up on the day someone actually needs it.
type Source interface {
	ScanHistory(ctx context.Context, ch drive.ChannelRef, visit func(Message) error) error
}

// RecoveredDir is where directories whose parent is missing are attached, so a
// partially damaged channel still yields reachable files.
const RecoveredDir = "_recovered"

// Progress reports rebuild advancement to the UI.
type Progress struct {
	Scanned    int    `json:"scanned"`
	Dirs       int    `json:"dirs"`
	Files      int    `json:"files"`
	Segments   int    `json:"segments"`
	Broken     int    `json:"broken"`
	Running    bool   `json:"running"`
	Done       bool   `json:"done"`
	Error      string `json:"error,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// Indexer runs at most one rebuild at a time.
type Indexer struct {
	db  *database.DB
	tg  Source
	log *zap.Logger

	// OnProgress is set by the API layer to forward progress over SSE.
	OnProgress func(Progress)

	mu      sync.Mutex
	running bool
	state   Progress
}

func New(db *database.DB, src Source, log *zap.Logger) *Indexer {
	return &Indexer{db: db, tg: src, log: log}
}

// Status returns the current or last rebuild's progress.
func (ix *Indexer) Status() Progress {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.state
}

// Start launches a rebuild in the background, refusing to run two at once.
func (ix *Indexer) Start(ctx context.Context) error {
	ix.mu.Lock()
	if ix.running {
		ix.mu.Unlock()
		return errors.New("an index rebuild is already running")
	}
	ix.running = true
	ix.state = Progress{Running: true, StartedAt: time.Now().UnixMilli()}
	ix.mu.Unlock()

	go func() {
		// The rebuild outlives the request that asked for it.
		runCtx := context.WithoutCancel(ctx)
		err := ix.rebuild(runCtx)

		ix.mu.Lock()
		ix.state.Running = false
		ix.state.Done = true
		ix.state.FinishedAt = time.Now().UnixMilli()
		if err != nil {
			ix.state.Error = err.Error()
		}
		final := ix.state
		ix.running = false
		ix.mu.Unlock()

		if err != nil {
			ix.log.Error("index rebuild failed", zap.Error(err))
		} else {
			ix.log.Info("index rebuild finished",
				zap.Int("dirs", final.Dirs), zap.Int("files", final.Files),
				zap.Int("segments", final.Segments), zap.Int("broken", final.Broken))
		}
		ix.publish(final)
	}()
	return nil
}

func (ix *Indexer) publish(p Progress) {
	if ix.OnProgress != nil {
		ix.OnProgress(p)
	}
}

func (ix *Indexer) advance(mutate func(*Progress)) {
	ix.mu.Lock()
	mutate(&ix.state)
	snapshot := ix.state
	ix.mu.Unlock()
	ix.publish(snapshot)
}

// scanned holds what one pass over the channel found.
type scanned struct {
	dirs  map[string]*dirRecord
	files map[string]*fileRecord
}

type dirRecord struct {
	rec   tagcodec.Record
	msgID int
}

type fileRecord struct {
	rec tagcodec.Record
	// segments maps a 1-based index to the document backing it. A caption
	// without a document is how an empty file is stored.
	segments map[int]segmentRecord
}

type segmentRecord struct {
	msgID int
	doc   *drive.StoredDoc
	size  int64
}

// rebuild scans the channel and replaces the index in one transaction.
func (ix *Indexer) rebuild(ctx context.Context) error {
	channel, err := ix.db.DefaultChannel(ctx)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return drive.ErrNoChannel
		}
		return err
	}

	found, err := ix.scan(ctx, channel)
	if err != nil {
		return err
	}

	dirs, err := ix.buildTree(found)
	if err != nil {
		return err
	}
	files, segments, broken := ix.buildFiles(found, dirs, channel)

	ix.advance(func(p *Progress) {
		p.Dirs, p.Files, p.Segments, p.Broken = len(dirs), len(files), len(segments), broken
	})

	return ix.replace(ctx, dirs, files, segments)
}

// scan walks the channel history, decoding every caption.
func (ix *Indexer) scan(ctx context.Context, channel database.Channel) (*scanned, error) {
	out := &scanned{
		dirs:  make(map[string]*dirRecord),
		files: make(map[string]*fileRecord),
	}

	total := 0
	err := ix.tg.ScanHistory(ctx, drive.ChannelRef{
		TGID:       channel.TGID,
		AccessHash: channel.AccessHash,
	}, func(msg Message) error {
		total++
		ix.absorb(out, msg)
		// Progress is reported per page rather than per message; a hundred
		// events a second would only flood the browser.
		if total%100 == 0 {
			ix.advance(func(p *Progress) { p.Scanned = total })
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read channel history: %w", err)
	}

	ix.advance(func(p *Progress) { p.Scanned = total })
	ix.log.Info("channel scan complete",
		zap.Int("messages", total),
		zap.Int("directory_records", len(out.dirs)),
		zap.Int("file_records", len(out.files)))
	return out, nil
}

// absorb decodes one message. Anything that is not a tdrive record is skipped
// silently: a storage channel may well contain messages a human sent.
func (ix *Indexer) absorb(out *scanned, m Message) {
	rec, err := tagcodec.Decode(m.Caption)
	if err != nil {
		if !errors.Is(err, tagcodec.ErrNotTagged) {
			ix.log.Debug("skipping an undecodable record",
				zap.Int("message", m.ID), zap.Error(err))
		}
		return
	}

	switch rec.Kind {
	case tagcodec.KindDir:
		// History is walked newest first, so an earlier entry is a later
		// edit of the same record and wins.
		if _, seen := out.dirs[rec.ID]; !seen {
			out.dirs[rec.ID] = &dirRecord{rec: rec, msgID: m.ID}
		}

	case tagcodec.KindFile:
		f, ok := out.files[rec.ID]
		if !ok {
			f = &fileRecord{rec: rec, segments: make(map[int]segmentRecord)}
			out.files[rec.ID] = f
		}
		if _, seen := f.segments[rec.SegIndex]; seen {
			return
		}

		seg := segmentRecord{msgID: m.ID}
		if m.Doc != nil {
			seg.doc, seg.size = m.Doc, m.Doc.Size
		}
		f.segments[rec.SegIndex] = seg
	}
}

// buildTree turns the flat directory records into rows with full paths.
//
// Parents may appear after children in the scan, and a parent may be missing
// entirely if its message was deleted. Both are handled by resolving paths
// iteratively and parking unresolvable subtrees under a recovery folder rather
// than dropping them.
func (ix *Indexer) buildTree(found *scanned) (map[string]database.Dir, error) {
	resolved := make(map[string]database.Dir, len(found.dirs))
	recoveredRoot := drive.Join(drive.Root, RecoveredDir)

	// place settles every record whose parent already has a path. Each pass
	// resolves at least one more level, so this converges in tree-depth
	// passes.
	place := func() {
		for progress := true; progress; {
			progress = false
			for id, d := range found.dirs {
				if _, done := resolved[id]; done {
					continue
				}
				parentPath := drive.Root
				if d.rec.ParentID != "" {
					parent, ok := resolved[d.rec.ParentID]
					if !ok {
						continue
					}
					parentPath = parent.Path
				}
				resolved[id] = database.Dir{
					ID:       id,
					ParentID: d.rec.ParentID,
					Name:     d.rec.Name,
					Path:     drive.Join(parentPath, d.rec.Name),
					TGMsgID:  d.msgID,
				}
				progress = true
			}
		}
	}
	place()

	// What is left either points at a parent whose message is gone, or sits
	// in a cycle that corrupted captions could produce. Re-rooting only the
	// topmost of those under a recovery folder and resolving again preserves
	// the shape of each rescued subtree, instead of flattening it.
	rescued := 0
	for {
		var tops []string
		for id, d := range found.dirs {
			if _, done := resolved[id]; done {
				continue
			}
			if _, parentExists := found.dirs[d.rec.ParentID]; parentExists {
				continue // its parent is still waiting; it is not a top
			}
			tops = append(tops, id)
		}

		if len(tops) == 0 {
			// Everything unresolved has a parent that also exists, which
			// means a cycle. Break it at the lowest id so the pass makes
			// progress rather than spinning.
			var lowest string
			for id := range found.dirs {
				if _, done := resolved[id]; done {
					continue
				}
				if lowest == "" || id < lowest {
					lowest = id
				}
			}
			if lowest == "" {
				break
			}
			ix.log.Warn("breaking a directory cycle found in the channel",
				zap.String("dir", found.dirs[lowest].rec.Name))
			tops = []string{lowest}
		}

		sort.Strings(tops)
		for _, id := range tops {
			d := found.dirs[id]
			if rescued == 0 {
				// The recovery folder itself needs a row, or its children
				// would point at a parent that does not exist.
				resolved[recoveredRootID] = database.Dir{
					ID:   recoveredRootID,
					Name: RecoveredDir,
					Path: recoveredRoot,
				}
			}
			resolved[id] = database.Dir{
				ID:       id,
				ParentID: recoveredRootID,
				Name:     d.rec.Name,
				Path:     drive.Join(recoveredRoot, d.rec.Name),
				TGMsgID:  d.msgID,
			}
			rescued++
		}
		place()
	}

	if rescued > 0 {
		ix.log.Warn("some directories lost their parent and were recovered",
			zap.Int("count", rescued), zap.String("under", recoveredRoot))
	}

	return ix.dedupePaths(resolved), nil
}

// recoveredRootID is a fixed, valid ULID for the synthetic recovery folder, so
// a rebuild that needs it twice produces the same row rather than a new one.
const recoveredRootID = "00000000000000000000RECVRD"

// dedupePaths resolves the name collisions that recovery can create, since two
// rescued directories may end up wanting the same slot.
//
// Paths are rebuilt from the parent down rather than patched in place, so a
// directory that has to be renamed drags its whole subtree with it instead of
// leaving children pointing at a path that no longer exists.
func (ix *Indexer) dedupePaths(dirs map[string]database.Dir) map[string]database.Dir {
	ids := make([]string, 0, len(dirs))
	for id := range dirs {
		ids = append(ids, id)
	}
	// Shallowest first so a parent's final path is known before its children
	// are placed; within a depth, ULID order means the oldest record keeps
	// the plain name.
	sort.Slice(ids, func(i, j int) bool {
		di, dj := len(drive.SplitPath(dirs[ids[i]].Path)), len(drive.SplitPath(dirs[ids[j]].Path))
		if di != dj {
			return di < dj
		}
		return ids[i] < ids[j]
	})

	final := make(map[string]string, len(dirs))
	taken := make(map[string]bool, len(dirs))

	for _, id := range ids {
		d := dirs[id]

		parentPath := drive.Root
		if d.ParentID != "" {
			if p, ok := final[d.ParentID]; ok {
				parentPath = p
			}
		}

		name := d.Name
		path := drive.Join(parentPath, name)
		for n := 2; taken[path]; n++ {
			name = fmt.Sprintf("%s (%d)", d.Name, n)
			path = drive.Join(parentPath, name)
		}

		taken[path] = true
		final[id] = path
		d.Name, d.Path = name, path
		dirs[id] = d
	}
	return dirs
}

// buildFiles turns the scanned file records into rows and segments, marking as
// broken anything whose segments do not add up.
func (ix *Indexer) buildFiles(
	found *scanned,
	dirs map[string]database.Dir,
	channel database.Channel,
) ([]database.File, []database.Segment, int) {
	var (
		files    []database.File
		segments []database.Segment
		broken   int
	)

	for id, f := range found.files {
		dirID := f.rec.ParentID
		if dirID != "" {
			if _, ok := dirs[dirID]; !ok {
				// The file's folder is gone; put it at the root rather than
				// losing it.
				dirID = ""
			}
		}

		status := database.StatusComplete
		if len(f.segments) != f.rec.SegCount {
			status = database.StatusBroken
			broken++
			ix.log.Warn("a file is missing segments",
				zap.String("name", f.rec.Name),
				zap.Int("found", len(f.segments)),
				zap.Int("expected", f.rec.SegCount))
		}

		files = append(files, database.File{
			ID:           id,
			DirID:        dirID,
			Name:         f.rec.Name,
			Size:         f.rec.TotalSize,
			MIME:         drive.GuessMIME(f.rec.Name),
			SegmentSize:  f.rec.SegmentSize,
			SegmentCount: f.rec.SegCount,
			Status:       status,
			ChannelID:    channel.ID,
			// Ownership travels in the caption, so a rebuild restores who
			// uploaded what and quota accounting survives. Captions written
			// before ownership existed simply carry no owner, and the column
			// stays NULL — which is honest, rather than attributing files to
			// whoever happens to run the rebuild.
			OwnerID: f.rec.OwnerID,
		})

		for idx, seg := range f.segments {
			row := database.Segment{
				FileID:  id,
				Index:   idx,
				Size:    seg.size,
				TGMsgID: seg.msgID,
			}
			if seg.doc != nil {
				row.TGDocID = seg.doc.DocID
				row.AccessHash = seg.doc.AccessHash
				row.FileReference = seg.doc.FileReference
				row.DCID = seg.doc.DCID
			}
			segments = append(segments, row)
		}
	}

	// Names collide when a rebuild rescues two files into the same folder.
	ix.dedupeFileNames(files)
	return files, segments, broken
}

func (ix *Indexer) dedupeFileNames(files []database.File) {
	sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })

	taken := make(map[string]bool, len(files))
	for i := range files {
		key := files[i].DirID + "/" + files[i].Name
		if !taken[key] {
			taken[key] = true
			continue
		}
		for n := 2; ; n++ {
			candidate := fmt.Sprintf("%s (%d)", files[i].Name, n)
			k := files[i].DirID + "/" + candidate
			if !taken[k] {
				taken[k] = true
				files[i].Name = candidate
				break
			}
		}
	}
}

// replace swaps in the rebuilt index atomically.
//
// The old rows are dropped and the new ones written inside a single
// transaction, so a rebuild that fails partway leaves the existing index
// untouched rather than half-erased.
func (ix *Indexer) replace(
	ctx context.Context,
	dirs map[string]database.Dir,
	files []database.File,
	segments []database.Segment,
) error {
	// Insert parents before children so the self-referencing foreign key on
	// dirs is satisfied at every step.
	ordered := make([]database.Dir, 0, len(dirs))
	for _, d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i].Path) != len(ordered[j].Path) {
			return len(ordered[i].Path) < len(ordered[j].Path)
		}
		return ordered[i].Path < ordered[j].Path
	})

	return ix.db.ReplaceIndex(ctx, ordered, files, segments)
}
