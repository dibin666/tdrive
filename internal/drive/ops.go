package drive

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/tagcodec"
)

// Renames and moves rewrite the caption of every message backing a record,
// because the caption is the durable copy of a file's name and location. For a
// seven-segment file that is seven edits — the cost of keeping the channel
// self-describing, which is what makes the index rebuildable.

// Rename changes the final component of a path.
func (s *Service) Rename(ctx context.Context, p, newName string) (Entry, error) {
	request := struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}{Path: p, Name: newName}
	operation, err := s.beforePluginOperation(ctx, "files.rename", request, &request)
	if err != nil {
		return Entry{}, err
	}
	p, newName = request.Path, request.Name
	if err := ValidateName(newName); err != nil {
		return Entry{}, err
	}
	clean, err := CleanPath(p)
	if err != nil {
		return Entry{}, err
	}
	if clean == Root {
		return Entry{}, errors.New("the drive root cannot be renamed")
	}

	parentPath, oldName := Parent(clean)
	if oldName == newName {
		entry, statErr := s.Stat(ctx, clean)
		if statErr == nil {
			s.afterPluginOperation(ctx, operation)
		}
		return entry, statErr
	}
	parent, err := s.ResolveDir(ctx, parentPath)
	if err != nil {
		return Entry{}, err
	}
	if err := s.checkFree(ctx, parent.ID, newName); err != nil {
		return Entry{}, err
	}

	if dir, err := s.db.DirByPath(ctx, clean); err == nil {
		entry, renameErr := s.renameDir(ctx, dir, newName, Join(parentPath, newName))
		if renameErr == nil {
			s.afterPluginOperation(ctx, operation)
		}
		return entry, renameErr
	} else if !errors.Is(err, database.ErrNotFound) {
		return Entry{}, err
	}

	file, err := s.db.FileInDir(ctx, parent.ID, oldName)
	if errors.Is(err, database.ErrNotFound) {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	if err != nil {
		return Entry{}, err
	}
	entry, renameErr := s.renameFile(ctx, file, newName, parentPath)
	if renameErr == nil {
		s.afterPluginOperation(ctx, operation)
	}
	return entry, renameErr
}

func (s *Service) renameDir(ctx context.Context, dir database.Dir, newName, newPath string) (Entry, error) {
	// SQLite first here, unusually: the descendant path rewrite is the part
	// that can fail on a uniqueness conflict, and doing it first means a
	// failure leaves Telegram untouched rather than half-edited.
	if err := s.db.RenameDir(ctx, dir.ID, newName, newPath); err != nil {
		if errors.Is(err, database.ErrConflict) {
			return Entry{}, fmt.Errorf("%w: %s", ErrExists, newPath)
		}
		return Entry{}, err
	}

	updated, err := s.db.DirByID(ctx, dir.ID)
	if err != nil {
		return Entry{}, err
	}
	if err := s.rewriteDirCaption(ctx, updated); err != nil {
		// The index is already correct and the caption is only consulted
		// during a rebuild, so a failed edit degrades recovery rather than
		// the running drive. Surfacing it as a warning beats undoing a
		// rename the user already saw succeed.
		s.log.Warn("could not update a directory caption after rename",
			zap.String("path", newPath), zap.Error(err))
	}

	// Descendant files carry their folder as readable hashtags, so a rename
	// higher up invalidates those captions too.
	s.refreshSubtreeCaptions(ctx, updated)
	return dirEntry(updated), nil
}

func (s *Service) renameFile(ctx context.Context, file database.File, newName, dirPath string) (Entry, error) {
	if err := s.db.RenameFile(ctx, file.ID, newName); err != nil {
		if errors.Is(err, database.ErrConflict) {
			return Entry{}, fmt.Errorf("%w: %s", ErrExists, newName)
		}
		return Entry{}, err
	}
	file.Name = newName

	if err := s.rewriteFileCaptions(ctx, file); err != nil {
		s.log.Warn("could not update file captions after rename",
			zap.String("file", file.ID), zap.Error(err))
	}
	return fileEntry(file, Join(dirPath, newName)), nil
}

// Move relocates a path into another directory.
func (s *Service) Move(ctx context.Context, from, toDir string) (Entry, error) {
	request := struct {
		From  string `json:"from"`
		ToDir string `json:"toDir"`
	}{From: from, ToDir: toDir}
	operation, err := s.beforePluginOperation(ctx, "files.move", request, &request)
	if err != nil {
		return Entry{}, err
	}
	from, toDir = request.From, request.ToDir
	src, err := CleanPath(from)
	if err != nil {
		return Entry{}, err
	}
	dstDir, err := CleanPath(toDir)
	if err != nil {
		return Entry{}, err
	}
	if src == Root {
		return Entry{}, errors.New("the drive root cannot be moved")
	}
	if IsDescendant(src, dstDir) || src == dstDir {
		return Entry{}, ErrLoop
	}

	_, name := Parent(src)
	target, err := s.Mkdir(ctx, dstDir)
	if err != nil {
		return Entry{}, err
	}
	if err := s.checkFree(ctx, target.ID, name); err != nil {
		return Entry{}, err
	}

	if dir, err := s.db.DirByPath(ctx, src); err == nil {
		newPath := Join(dstDir, name)
		if err := s.db.MoveDir(ctx, dir.ID, target.ID, newPath); err != nil {
			if errors.Is(err, database.ErrConflict) {
				return Entry{}, fmt.Errorf("%w: %s", ErrExists, newPath)
			}
			return Entry{}, err
		}
		updated, err := s.db.DirByID(ctx, dir.ID)
		if err != nil {
			return Entry{}, err
		}
		if err := s.rewriteDirCaption(ctx, updated); err != nil {
			s.log.Warn("could not update a directory caption after move",
				zap.String("path", newPath), zap.Error(err))
		}
		s.refreshSubtreeCaptions(ctx, updated)
		entry := dirEntry(updated)
		s.afterPluginOperation(ctx, operation)
		return entry, nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return Entry{}, err
	}

	parentPath, _ := Parent(src)
	parent, err := s.ResolveDir(ctx, parentPath)
	if err != nil {
		return Entry{}, err
	}
	file, err := s.db.FileInDir(ctx, parent.ID, name)
	if errors.Is(err, database.ErrNotFound) {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, src)
	}
	if err != nil {
		return Entry{}, err
	}

	if err := s.db.MoveFile(ctx, file.ID, target.ID); err != nil {
		if errors.Is(err, database.ErrConflict) {
			return Entry{}, fmt.Errorf("%w: %s", ErrExists, Join(dstDir, name))
		}
		return Entry{}, err
	}
	file.DirID = target.ID
	if err := s.rewriteFileCaptions(ctx, file); err != nil {
		s.log.Warn("could not update file captions after move",
			zap.String("file", file.ID), zap.Error(err))
	}
	entry := fileEntry(file, Join(dstDir, name))
	s.afterPluginOperation(ctx, operation)
	return entry, nil
}

// Delete removes a file or a whole directory subtree, taking the Telegram
// messages with it.
//
// Telegram is cleared first. The index rows are the map to those messages, so
// dropping them first would strand every document in the channel with nothing
// pointing at it.
func (s *Service) Delete(ctx context.Context, p string) error {
	request := struct {
		Path string `json:"path"`
	}{Path: p}
	operation, err := s.beforePluginOperation(ctx, "files.delete", request, &request)
	if err != nil {
		return err
	}
	clean, err := CleanPath(request.Path)
	if err != nil {
		return err
	}
	if clean == Root {
		return errors.New("the drive root cannot be deleted")
	}

	if dir, err := s.db.DirByPath(ctx, clean); err == nil {
		msgs, err := s.db.SubtreeMessages(ctx, dir.ID)
		if err != nil {
			return err
		}
		if err := s.deleteMessages(ctx, msgs); err != nil {
			return err
		}
		err = s.db.DeleteDir(ctx, dir.ID)
		if err == nil {
			s.afterPluginOperation(ctx, operation)
		}
		return err
	} else if !errors.Is(err, database.ErrNotFound) {
		return err
	}

	parentPath, name := Parent(clean)
	parent, err := s.ResolveDir(ctx, parentPath)
	if err != nil {
		return err
	}
	file, err := s.db.FileInDir(ctx, parent.ID, name)
	if errors.Is(err, database.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	if err != nil {
		return err
	}
	err = s.deleteFileRow(ctx, file)
	if err == nil {
		s.afterPluginOperation(ctx, operation)
	}
	return err
}

// DeleteFileByID removes one file, used by the API where the client already
// holds an id.
func (s *Service) DeleteFileByID(ctx context.Context, id string) error {
	request := struct {
		ID string `json:"id"`
	}{ID: id}
	operation, err := s.beforePluginOperation(ctx, "files.deleteByID", request, &request)
	if err != nil {
		return err
	}
	id = request.ID
	file, err := s.db.FileByID(ctx, id)
	if err != nil {
		return err
	}
	err = s.deleteFileRow(ctx, file)
	if err == nil {
		s.afterPluginOperation(ctx, operation)
	}
	return err
}

func (s *Service) deleteFileRow(ctx context.Context, file database.File) error {
	msgs, err := s.db.FileMessages(ctx, file.ID)
	if err != nil {
		return err
	}
	if err := s.deleteMessages(ctx, msgs); err != nil {
		return err
	}
	s.refs.Forget(file.ID)
	return s.db.DeleteFile(ctx, file.ID)
}

// deleteMessages groups messages by channel so each channel takes one batched
// call, which matters when a directory holds thousands of segments.
func (s *Service) deleteMessages(ctx context.Context, msgs []database.TGMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	byChannel := make(map[string][]int)
	for _, m := range msgs {
		if m.MsgID > 0 {
			byChannel[m.ChannelID] = append(byChannel[m.ChannelID], m.MsgID)
		}
	}

	// Deletes are cheap metadata calls, so they take no transfer slot. Any
	// account can remove another's messages: every account is granted delete
	// rights when it is admitted to the channel, precisely so that a rename or
	// a delete never has to wait for the account that happened to upload.
	account, err := s.metaAccount(ctx)
	if err != nil {
		return err
	}
	for channelID, ids := range byChannel {
		ch, err := s.channelFor(ctx, channelID)
		if err != nil {
			return err
		}
		chRef, err := channelRef(ctx, account, ch)
		if err != nil {
			return err
		}
		if err := account.DeleteRecords(ctx, chRef, ids); err != nil {
			return err
		}
	}
	return nil
}

// checkFree rejects a name already taken by either a file or a directory.
func (s *Service) checkFree(ctx context.Context, dirID, name string) error {
	if _, err := s.db.DirChild(ctx, dirID, name); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, name)
	} else if !errors.Is(err, database.ErrNotFound) {
		return err
	}
	if _, err := s.db.FileInDir(ctx, dirID, name); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, name)
	} else if !errors.Is(err, database.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Service) rewriteDirCaption(ctx context.Context, dir database.Dir) error {
	if dir.TGMsgID == 0 {
		return nil
	}
	channel, err := s.channelFor(ctx, dir.ChannelID)
	if err != nil {
		return err
	}
	caption, err := tagcodec.EncodeDir(dir.ID, dir.ParentID, dir.Name, dir.Path)
	if err != nil {
		return err
	}
	account, err := s.metaAccount(ctx)
	if err != nil {
		return err
	}
	chRef, err := channelRef(ctx, account, channel)
	if err != nil {
		return err
	}
	return account.EditRecord(ctx, chRef, dir.TGMsgID, caption)
}

// rewriteFileCaptions updates every segment's caption after a rename or move.
func (s *Service) rewriteFileCaptions(ctx context.Context, file database.File) error {
	segs, err := s.db.Segments(ctx, file.ID)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return nil
	}
	channel, err := s.channelFor(ctx, file.ChannelID)
	if err != nil {
		return err
	}
	account, err := s.metaAccount(ctx)
	if err != nil {
		return err
	}
	chRef, err := channelRef(ctx, account, channel)
	if err != nil {
		return err
	}

	// Editing a message invalidates its file reference, so anything cached
	// for this file has to go — for every account, since each one holds its
	// own handles.
	s.refs.Forget(file.ID)

	for _, seg := range segs {
		caption, err := s.fileCaption(ctx, file, seg.Index)
		if err != nil {
			return err
		}
		if err := account.EditRecord(ctx, chRef, seg.TGMsgID, caption); err != nil {
			return err
		}
	}
	return nil
}

// refreshSubtreeCaptions rewrites the captions of files under a directory that
// moved or was renamed, because their readable folder hashtags are now stale.
//
// This runs in the background: a deep subtree can hold thousands of messages,
// and the user should not wait on Telegram edits for a rename that has already
// taken effect everywhere that matters.
func (s *Service) refreshSubtreeCaptions(ctx context.Context, dir database.Dir) {
	go func() {
		// The request context dies with the HTTP response; this work
		// deliberately outlives it.
		ctx := context.WithoutCancel(ctx)

		dirs, err := s.db.AllDirs(ctx)
		if err != nil {
			s.log.Warn("could not enumerate directories for caption refresh", zap.Error(err))
			return
		}
		for _, d := range dirs {
			if d.ID != dir.ID && !IsDescendant(dir.Path, d.Path) {
				continue
			}
			files, err := s.db.ListFiles(ctx, d.ID)
			if err != nil {
				s.log.Warn("could not list files for caption refresh",
					zap.String("dir", d.Path), zap.Error(err))
				continue
			}
			for _, f := range files {
				if err := s.rewriteFileCaptions(ctx, f); err != nil {
					s.log.Warn("could not refresh a file's captions",
						zap.String("file", f.ID), zap.Error(err))
				}
			}
		}
	}()
}
