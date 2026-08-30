package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
)

// Every handler here converts the client's paths into real drive paths on the
// way in and back on the way out. An unscoped account — which is every account
// by default, and every administrator always — pays nothing for this: the
// conversion is the identity function and the drive sees exactly what it saw
// before.

type listBody struct {
	Path    string        `json:"path"`
	Entries []drive.Entry `json:"entries"`
	// Breadcrumbs let the UI render the path without re-splitting it.
	Breadcrumbs []crumb `json:"breadcrumbs"`
}

type crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	visible := r.URL.Query().Get("path")
	if visible == "" {
		visible = drive.Root
	}
	real, err := s.realPath(r, visible)
	if err != nil {
		s.fail(w, err, "list")
		return
	}

	entries, err := s.drive.List(r.Context(), real)
	if err != nil {
		s.fail(w, err, "list")
		return
	}
	scope := s.scopeOf(r)
	entries = scope.ApplyAll(entries)
	if entries == nil {
		entries = []drive.Entry{}
	}

	shown, ok := scope.ToVisible(real)
	if !ok {
		s.fail(w, fmt.Errorf("%w: %s", drive.ErrOutOfScope, visible), "list")
		return
	}
	writeJSON(w, http.StatusOK, listBody{
		Path:        shown,
		Entries:     entries,
		Breadcrumbs: breadcrumbs(shown),
	})
}

func breadcrumbs(p string) []crumb {
	out := []crumb{{Name: "", Path: drive.Root}}
	acc := ""
	for _, part := range drive.SplitPath(p) {
		acc = drive.Join(acc, part)
		out = append(out, crumb{Name: part, Path: acc})
	}
	return out
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	real, err := s.realPath(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err, "stat")
		return
	}
	entry, err := s.drive.Stat(r.Context(), real)
	if err != nil {
		s.fail(w, err, "stat")
		return
	}
	scoped, ok := s.scopeOf(r).Apply(entry)
	if !ok {
		s.fail(w, fmt.Errorf("%w: %s", drive.ErrOutOfScope, entry.Path), "stat")
		return
	}
	writeJSON(w, http.StatusOK, scoped)
}

type pathRequest struct {
	Path string `json:"path"`
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req pathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	real, err := s.realPath(r, req.Path)
	if err != nil {
		s.fail(w, err, "mkdir")
		return
	}
	dir, err := s.drive.Mkdir(r.Context(), real)
	if err != nil {
		s.fail(w, err, "mkdir")
		return
	}
	parent, _ := drive.Parent(dir.Path)
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: parent}})

	if visible, err := s.visiblePath(r, dir.Path); err == nil {
		dir.Path = visible
	}
	writeJSON(w, http.StatusCreated, dir)
}

type renameRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	real, err := s.realPath(r, req.Path)
	if err != nil {
		s.fail(w, err, "rename")
		return
	}
	entry, err := s.drive.Rename(r.Context(), real, req.Name)
	if err != nil {
		s.fail(w, err, "rename")
		return
	}
	parent, _ := drive.Parent(entry.Path)
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: parent}})

	scoped, _ := s.scopeOf(r).Apply(entry)
	writeJSON(w, http.StatusOK, scoped)
}

// batchRenameRequest renames several entries in one call.
//
// A loop of single renames in the browser cannot do this correctly: swapping
// two names, or shifting a numbered series down by one, transiently collides
// with a name that is about to move out of the way. Doing it server-side means
// the whole set can be validated first and the collisions broken with
// temporary names, so either every rename lands or none of them do.
type batchRenameRequest struct {
	Items []batchRenameItem `json:"items"`
}

type batchRenameItem struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type batchRenameResult struct {
	Path string `json:"path"`
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// NewPath is the entry's path after the rename, so the client can update
	// its selection without reloading.
	NewPath string `json:"newPath,omitempty"`
	Error   string `json:"error,omitempty"`
}

// MaxBatchRename bounds one request. A rename edits every Telegram caption
// backing an entry, so a thousand-item batch is thousands of RPCs; the limit
// keeps one request from monopolising the rate budget.
const MaxBatchRename = 500

func (s *Server) handleBatchRename(w http.ResponseWriter, r *http.Request) {
	var req batchRenameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "没有要重命名的项目")
		return
	}
	if len(req.Items) > MaxBatchRename {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("一次最多重命名 %d 项", MaxBatchRename))
		return
	}

	type plan struct {
		realPath string
		parent   string
		oldName  string
		newName  string
		target   string
	}

	// Phase one: validate everything before touching anything. A batch that is
	// going to fail on item forty should fail before item one has been
	// rewritten in Telegram.
	plans := make([]plan, 0, len(req.Items))
	seen := make(map[string]bool, len(req.Items))
	targets := make(map[string]bool, len(req.Items))

	for _, item := range req.Items {
		real, err := s.realPath(r, item.Path)
		if err != nil {
			s.fail(w, err, "batch rename")
			return
		}
		if real == drive.Root {
			writeError(w, http.StatusBadRequest, "不能重命名根目录")
			return
		}
		name := strings.TrimSpace(item.Name)
		if err := drive.ValidateName(name); err != nil {
			s.fail(w, err, "batch rename")
			return
		}
		if seen[real] {
			writeError(w, http.StatusBadRequest, "同一项在一次批量重命名里出现了两次")
			return
		}
		seen[real] = true
		exists, err := s.pathExists(r.Context(), real)
		if err != nil {
			// Resolve every source before the first temporary rename. This is
			// what keeps a stale later item from leaving earlier items parked.
			s.fail(w, err, "batch rename")
			return
		}
		if !exists {
			// pathExists also sees pending files, while drive.Stat hides them
			// from ordinary reads. Rename validation should resolve the actual
			// source entry rather than applying read visibility rules.
			s.fail(w, fmt.Errorf("%w: %s", drive.ErrNotFound, real), "batch rename")
			return
		}

		parent, oldName := drive.Parent(real)
		target := drive.Join(parent, name)
		if targets[target] {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("重命名后会有两项都叫 %q", name))
			return
		}
		targets[target] = true

		plans = append(plans, plan{
			realPath: real, parent: parent, oldName: oldName, newName: name, target: target,
		})
	}

	// Finish checking the target side before touching anything. A target that is
	// not one of the sources must be free now; otherwise a later rename would
	// fail after earlier items had already changed the tree.
	sourceIndex := make(map[string]int, len(plans))
	for i, p := range plans {
		sourceIndex[p.realPath] = i
	}
	for _, p := range plans {
		if _, moving := sourceIndex[p.target]; moving {
			continue
		}
		taken, err := s.pathExists(r.Context(), p.target)
		if err != nil {
			s.fail(w, err, "batch rename")
			return
		}
		if taken {
			writeError(w, http.StatusConflict, fmt.Sprintf("目标名称 %q 已存在", p.newName))
			return
		}
	}

	// Phase two: break cycles. If a source is the current target of another
	// item, that source is parked first. Parking the occupied destination (not
	// the item whose target points at it) also handles chains such as A→B,
	// B→C; leaving B in place would make A→B collide.
	//
	// A swap of A and B is the smallest case, but a rotation of any length
	// behaves the same way, and detecting the cycle shape precisely buys
	// nothing over parking every conflicting entry.
	parkNames := make(map[int]string)
	for i, p := range plans {
		j, moving := sourceIndex[p.target]
		if !moving || j == i {
			continue
		}
		if _, already := parkNames[j]; already {
			continue
		}
		for {
			temp := fmt.Sprintf(".tdrive-rename-%s", database.NewID())
			taken, err := s.pathExists(r.Context(), drive.Join(plans[j].parent, temp))
			if err != nil {
				s.fail(w, err, "batch rename")
				return
			}
			if !taken {
				parkNames[j] = temp
				break
			}
		}
	}

	parked := make(map[int]string, len(parkNames))
	for i, temp := range parkNames {
		if _, err := s.drive.Rename(r.Context(), plans[i].realPath, temp); err != nil {
			for parkedIndex, parkedPath := range parked {
				if _, restoreErr := s.drive.Rename(r.Context(), parkedPath, plans[parkedIndex].oldName); restoreErr != nil {
					s.log.Warn("could not roll back a temporary batch rename",
						zap.String("path", parkedPath), zap.Error(restoreErr))
				}
			}
			s.fail(w, err, "batch rename")
			return
		}
		parked[i] = drive.Join(plans[i].parent, temp)
	}

	results := make([]batchRenameResult, 0, len(plans))
	touched := map[string]bool{}
	applied := make([]int, 0, len(plans))

	for i, p := range plans {
		from := p.realPath
		if temp, ok := parked[i]; ok {
			from = temp
		}
		visibleOld, _ := s.scopeOf(r).ToVisible(p.realPath)

		entry, err := s.drive.Rename(r.Context(), from, p.newName)
		if err != nil {
			// The preflight should make this exceptional, but a concurrent
			// mutation or an external backend failure can still happen. Undo
			// completed moves and unpark the rest before reporting an error.
			appliedSet := make(map[int]bool, len(applied))
			for _, appliedIndex := range applied {
				appliedSet[appliedIndex] = true
			}
			for n := len(applied) - 1; n >= 0; n-- {
				appliedIndex := applied[n]
				if _, restoreErr := s.drive.Rename(r.Context(), plans[appliedIndex].target, plans[appliedIndex].oldName); restoreErr != nil {
					s.log.Warn("could not roll back a batch rename",
						zap.String("path", plans[appliedIndex].target), zap.Error(restoreErr))
				}
			}
			for parkedIndex, parkedPath := range parked {
				if appliedSet[parkedIndex] {
					continue
				}
				if _, restoreErr := s.drive.Rename(r.Context(), parkedPath, plans[parkedIndex].oldName); restoreErr != nil {
					s.log.Warn("could not roll back a parked batch rename",
						zap.String("path", parkedPath), zap.Error(restoreErr))
				}
			}
			s.fail(w, err, "batch rename")
			return
		}
		applied = append(applied, i)
		touched[p.parent] = true
		visibleNew, _ := s.scopeOf(r).ToVisible(entry.Path)
		results = append(results, batchRenameResult{
			Path: visibleOld, Name: p.newName, OK: true, NewPath: visibleNew,
		})
	}

	// One event for the whole batch rather than one per item: the client
	// reloads the listing once either way.
	for p := range touched {
		s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: p}})
	}
	s.audit(r, database.AuditFileBatchRename, "",
		fmt.Sprintf("items=%d failed=0", len(plans)))

	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"renamed": len(plans),
		"failed":  0,
	})
}

// pathExists checks both directories and files, including pending files that
// drive.Stat intentionally hides from normal reads. Batch rename must account
// for every occupied destination before it starts mutating the tree.
func (s *Server) pathExists(ctx context.Context, real string) (bool, error) {
	if _, err := s.db.DirByPath(ctx, real); err == nil {
		return true, nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return false, err
	}

	parentPath, name := drive.Parent(real)
	parent, err := s.drive.ResolveDir(ctx, parentPath)
	if err != nil {
		return false, err
	}
	if _, err := s.db.FileInDir(ctx, parent.ID, name); err == nil {
		return true, nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return false, err
	}
	return false, nil
}

type moveRequest struct {
	Path string `json:"path"`
	To   string `json:"to"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	source, err := s.realPath(r, req.Path)
	if err != nil {
		s.fail(w, err, "move")
		return
	}
	destination, err := s.realPath(r, req.To)
	if err != nil {
		s.fail(w, err, "move")
		return
	}

	from, _ := drive.Parent(source)
	entry, err := s.drive.Move(r.Context(), source, destination)
	if err != nil {
		s.fail(w, err, "move")
		return
	}
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: from}})
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: destination}})

	scoped, _ := s.scopeOf(r).Apply(entry)
	writeJSON(w, http.StatusOK, scoped)
}

type deleteRequest struct {
	// Paths accepts several targets so a multi-select in the UI is one
	// request rather than a burst of them.
	Paths []string `json:"paths"`
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "no paths given")
		return
	}

	touched := map[string]bool{}
	for _, p := range req.Paths {
		real, err := s.realPath(r, p)
		if err != nil {
			s.fail(w, err, "delete")
			return
		}
		if err := s.drive.Delete(r.Context(), real); err != nil {
			s.fail(w, err, "delete")
			return
		}
		parent, _ := drive.Parent(real)
		touched[parent] = true
	}
	for p := range touched {
		s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: p}})
	}
	s.audit(r, database.AuditFileDelete, strings.Join(req.Paths, ", "), "")
	w.WriteHeader(http.StatusNoContent)
}

// segmentInfo exposes a file's physical layout for the details panel. It is
// informational only: every other endpoint treats the file as one object.
type segmentInfo struct {
	Index int   `json:"index"`
	Size  int64 `json:"size"`
	// MessageID is shown so a user can find the segment in their own
	// Telegram client.
	MessageID int `json:"messageId"`
}

func (s *Server) handleSegments(w http.ResponseWriter, r *http.Request) {
	file, err := s.fileForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "segments")
		return
	}
	segs, err := s.db.Segments(r.Context(), file.ID)
	if err != nil {
		s.fail(w, err, "segments")
		return
	}

	out := make([]segmentInfo, 0, len(segs))
	for _, seg := range segs {
		out = append(out, segmentInfo{Index: seg.Index, Size: seg.Size, MessageID: seg.TGMsgID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file":     file,
		"segments": out,
	})
}
