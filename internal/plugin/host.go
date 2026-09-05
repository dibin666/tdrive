package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// managerHost is the concrete implementation of the public Host bridge. A host
// instance is scoped to one account's installation of one plugin, so plugin
// data cannot overlap with another plugin's namespace — nor with the same
// plugin installed by somebody else.
type managerHost struct {
	manager  *Manager
	pluginID string
	// userID is the account that installed this plugin. It is what makes the
	// data namespace personal and what the two administrator-only host methods
	// are checked against.
	userID string
}

// owner loads the account that installed this plugin. It is read on demand
// rather than captured at attach time because a role can change while a plugin
// is running, and the answer that matters is the one at the moment of the call.
func (host *managerHost) owner(ctx context.Context) (database.User, error) {
	if host.userID == "" {
		return database.User{}, errors.New("插件缺少所有者账号。")
	}
	return host.manager.db.UserByID(ctx, host.userID)
}

// requireAdminOwner gates the host methods whose HTTP equivalents are
// administrator-only. A plugin acts for its owner, so it must not be a way to
// reach something the owner could not reach through the WebUI.
func (host *managerHost) requireAdminOwner(ctx context.Context) error {
	owner, err := host.owner(ctx)
	if err != nil {
		return err
	}
	if owner.Role != database.RoleAdmin {
		return errors.New("插件所有者非管理员，无权读取或修改运行参数。")
	}
	return nil
}

func (host *managerHost) Call(ctx context.Context, method string, request any, response any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = tdriveplugin.WithHostCall(ctx)
	var requestData json.RawMessage
	switch value := request.(type) {
	case nil:
		requestData = json.RawMessage("{}")
	case json.RawMessage:
		requestData = value
	case []byte:
		requestData = value
	default:
		encoded, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("encode host request: %w", err)
		}
		requestData = encoded
	}

	result, err := host.dispatch(ctx, method, requestData)
	if err != nil {
		return err
	}
	if response == nil || len(result) == 0 {
		return nil
	}
	if target, ok := response.(*json.RawMessage); ok {
		*target = append((*target)[:0], result...)
		return nil
	}
	if err := json.Unmarshal(result, response); err != nil {
		return fmt.Errorf("decode host response: %w", err)
	}
	return nil
}

func (host *managerHost) dispatch(ctx context.Context, method string, request json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "files.list":
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		if input.Path == "" {
			input.Path = drive.Root
		}
		entries, err := host.manager.drive.List(ctx, input.Path)
		if err != nil {
			return nil, err
		}
		publicEntries := make([]tdriveplugin.Entry, 0, len(entries))
		for _, entry := range entries {
			publicEntries = append(publicEntries, publicEntry(entry))
		}
		return encodeHostResponse(publicEntries)

	case "files.stat":
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		entry, err := host.manager.drive.Stat(ctx, input.Path)
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(publicEntry(entry))

	case "files.mkdir":
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		directory, err := host.manager.drive.Mkdir(ctx, input.Path)
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(directory)

	case "files.rename":
		var input struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		entry, err := host.manager.drive.Rename(ctx, input.Path, input.Name)
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(publicEntry(entry))

	case "files.move":
		var input struct {
			From  string `json:"from"`
			ToDir string `json:"toDir"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		entry, err := host.manager.drive.Move(ctx, input.From, input.ToDir)
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(publicEntry(entry))

	case "files.delete":
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		return nil, host.manager.drive.Delete(ctx, input.Path)

	case "files.beginUpload":
		var input tdriveplugin.UploadRequest
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		job, file, err := host.manager.drive.Begin(ctx, drive.UploadRequest{
			DirPath:   input.DirPath,
			Name:      input.Name,
			Size:      input.Size,
			MIME:      input.MIME,
			UserID:    input.UserID,
			Source:    input.Source,
			SourceURL: input.SourceURL,
			Overwrite: input.Overwrite,
		})
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(struct {
			Job  tdriveplugin.UploadJob `json:"job"`
			File tdriveplugin.File      `json:"file"`
		}{Job: publicUploadJob(job), File: publicFile(file)})

	case "files.completeUpload":
		var input struct {
			JobID string `json:"jobId"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		file, err := host.manager.drive.Complete(ctx, input.JobID)
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(publicFile(file))

	case "files.abortUpload":
		var input struct {
			JobID  string `json:"jobId"`
			Reason string `json:"reason"`
			State  string `json:"state"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		state := database.JobCancelled
		if input.State == string(database.JobFailed) {
			state = database.JobFailed
		}
		return nil, host.manager.drive.Abort(ctx, input.JobID, input.Reason, state)

	case "downloads.stage":
		var input struct {
			FileID string `json:"fileId"`
			UserID string `json:"userId"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		job, err := host.manager.drive.StartStaged(ctx, drive.StageRequest{FileID: input.FileID, UserID: input.UserID})
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(publicDownloadJob(job))

	case "downloads.cancel":
		var input struct {
			JobID string `json:"jobId"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		return nil, host.manager.drive.CancelStaged(ctx, input.JobID)

	case "files.readChunk":
		var input struct {
			FileID string `json:"fileId"`
			Offset int64  `json:"offset"`
			Size   int64  `json:"size"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		if input.Offset < 0 || input.Size < 0 || input.Size > 16<<20 {
			return nil, errors.New("readChunk offset or size is invalid")
		}
		file, err := host.manager.db.FileByID(ctx, input.FileID)
		if err != nil {
			return nil, err
		}
		account, err := host.manager.drive.ReadAccount(ctx, file.ID)
		if err != nil {
			return nil, err
		}
		reader, err := host.manager.drive.OpenFile(ctx, file, account)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		if _, err := reader.Seek(input.Offset, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(reader, input.Size))
		if err != nil {
			return nil, err
		}
		return encodeHostResponse(struct {
			Data []byte `json:"data"`
		}{Data: data})

	case "users.list":
		// A plugin belongs to one account, so the host tells it about that
		// account and no other. Enumerating the user table was harmless while
		// only an administrator could install a plugin; the moment anyone else
		// can, it is an account directory handed to third-party code.
		owner, err := host.owner(ctx)
		if err != nil {
			return nil, err
		}
		return encodeHostResponse([]tdriveplugin.User{{
			ID: owner.ID, Username: owner.Username, Role: string(owner.Role), Enabled: owner.Enabled,
		}})

	case "settings.get":
		if err := host.requireAdminOwner(ctx); err != nil {
			return nil, err
		}
		return encodeHostResponse(host.manager.cfg.RuntimeSettings())

	case "settings.update":
		if err := host.requireAdminOwner(ctx); err != nil {
			return nil, err
		}
		var updates map[string]json.RawMessage
		if err := decodeHostRequest(request, &updates); err != nil {
			return nil, err
		}
		if err := host.updateRuntimeSettings(ctx, updates); err != nil {
			return nil, err
		}
		return encodeHostResponse(host.manager.cfg.RuntimeSettings())

	case "telegram.status":
		if host.manager.tg == nil {
			return encodeHostResponse(nil)
		}
		return encodeHostResponse(host.manager.tg.Status())

	case "events.publish":
		var input struct {
			Type   string          `json:"type"`
			Data   json.RawMessage `json:"data"`
			UserID string          `json:"userId,omitempty"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		if input.Type == "" {
			return nil, errors.New("event type is required")
		}
		host.manager.broker.Publish(events.Event{Type: events.Type(input.Type), Data: input.Data, UserID: input.UserID})
		return nil, nil

	case "data.get":
		var input struct {
			Key string `json:"key"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		if err := validatePluginDataKey(input.Key); err != nil {
			return nil, err
		}
		value, err := host.manager.db.PluginData(ctx, host.userID, host.pluginID, input.Key)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(value), nil

	case "data.set":
		var input struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		if err := validatePluginDataKey(input.Key); err != nil {
			return nil, err
		}
		return nil, host.manager.db.SetPluginData(ctx, host.userID, host.pluginID, input.Key, input.Value)

	case "data.delete":
		var input struct {
			Key string `json:"key"`
		}
		if err := decodeHostRequest(request, &input); err != nil {
			return nil, err
		}
		if err := validatePluginDataKey(input.Key); err != nil {
			return nil, err
		}
		return nil, host.manager.db.DeletePluginData(ctx, host.userID, host.pluginID, input.Key)

	default:
		return nil, fmt.Errorf("unknown host method %q", method)
	}
}

func (host *managerHost) OpenStream(ctx context.Context, method string, request any) (io.ReadWriteCloser, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = tdriveplugin.WithHostCall(ctx)
	var requestData json.RawMessage
	switch value := request.(type) {
	case json.RawMessage:
		requestData = value
	case []byte:
		requestData = value
	default:
		encoded, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		requestData = encoded
	}
	if method == "files.putSegment" {
		var input struct {
			JobID string `json:"jobId"`
			Index int    `json:"index"`
			Size  int64  `json:"size"`
		}
		if err := decodeHostRequest(requestData, &input); err != nil {
			return nil, err
		}
		if input.Index < 1 || input.Size < 0 || input.Size > 2<<30 {
			return nil, errors.New("upload stream index or size is invalid")
		}
		job, err := host.manager.db.JobByID(ctx, input.JobID)
		if err != nil {
			return nil, err
		}
		// A plugin drives its upload from its own goroutine, so cancelling the
		// transfer in the WebUI has nothing to interrupt unless the segment is
		// registered against the job. Without this the plugin kept pushing bytes
		// into a transfer the panel had already recorded as cancelled.
		segmentCtx, release := host.manager.drive.WatchUploadJob(ctx, input.JobID)
		stream := newUploadStream()
		go func() {
			defer release()
			// PutSegment itself is deliberately a low-level primitive. The
			// browser normally wraps it in AcquireUploadJob, but plugin streams
			// arrive through this bridge, so acquire the same job-level account
			// lease here. This keeps plugin segments on one account and makes
			// daily quota exhaustion apply to them as well.
			_, releaseRequest, err := host.manager.drive.AcquireUploadJob(segmentCtx, input.JobID)
			if err == nil {
				defer releaseRequest()
				err = host.manager.drive.PutSegment(segmentCtx, job, input.Index, stream.reader(), input.Size, nil)
			}
			stream.finish(err)
		}()
		return stream, nil
	}
	if method != "files.read" {
		return nil, fmt.Errorf("unknown host stream method %q", method)
	}
	var input struct {
		FileID string `json:"fileId"`
		Offset int64  `json:"offset"`
		Size   int64  `json:"size"`
	}
	if err := decodeHostRequest(requestData, &input); err != nil {
		return nil, err
	}
	if input.Offset < 0 || input.Size < 0 || input.Size > 64<<20 {
		return nil, errors.New("read stream offset or size is invalid")
	}
	file, err := host.manager.db.FileByID(ctx, input.FileID)
	if err != nil {
		return nil, err
	}
	account, err := host.manager.drive.ReadAccount(ctx, file.ID)
	if err != nil {
		return nil, err
	}
	reader, err := host.manager.drive.OpenFile(ctx, file, account)
	if err != nil {
		return nil, err
	}
	if _, err := reader.Seek(input.Offset, io.SeekStart); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return &limitedReadStream{reader: reader, remaining: input.Size}, nil
}

type limitedReadStream struct {
	reader    io.ReadCloser
	remaining int64
}

// uploadStream turns the brokered connection into a streaming segment upload.
// Closing the write side sends EOF to the drive and waits for the segment's
// database commit, so the plugin sees validation or Telegram errors on Close.
type uploadStream struct {
	pipeReader *io.PipeReader
	pipeWriter *io.PipeWriter
	done       chan struct{}

	mu        sync.Mutex
	err       error
	closeOnce sync.Once
}

func newUploadStream() *uploadStream {
	reader, writer := io.Pipe()
	return &uploadStream{pipeReader: reader, pipeWriter: writer, done: make(chan struct{})}
}

func (stream *uploadStream) reader() io.Reader { return stream.pipeReader }

func (stream *uploadStream) finish(err error) {
	stream.mu.Lock()
	stream.err = err
	stream.mu.Unlock()
	close(stream.done)
}

func (stream *uploadStream) Read(buffer []byte) (int, error) {
	<-stream.done
	stream.mu.Lock()
	err := stream.err
	stream.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}

func (stream *uploadStream) Write(buffer []byte) (int, error) {
	return stream.pipeWriter.Write(buffer)
}

func (stream *uploadStream) Close() error {
	stream.closeOnce.Do(func() {
		_ = stream.pipeWriter.Close()
	})
	<-stream.done
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.err
}

func (stream *limitedReadStream) Read(buffer []byte) (int, error) {
	if stream.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > stream.remaining {
		buffer = buffer[:int(stream.remaining)]
	}
	count, err := stream.reader.Read(buffer)
	stream.remaining -= int64(count)
	if stream.remaining == 0 && err == nil {
		err = io.EOF
	}
	return count, err
}

// Writes to a read stream are accepted and discarded so the reverse half of
// the brokered duplex connection can finish without turning a download into a
// protocol error.
func (stream *limitedReadStream) Write(buffer []byte) (int, error) { return len(buffer), nil }

func (stream *limitedReadStream) Close() error { return stream.reader.Close() }

func (host *managerHost) updateRuntimeSettings(ctx context.Context, updates map[string]json.RawMessage) error {
	currentData, err := json.Marshal(host.manager.cfg.RuntimeSettings())
	if err != nil {
		return err
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(currentData, &current); err != nil {
		return err
	}
	for key, value := range updates {
		current[key] = value
	}
	merged, err := json.Marshal(current)
	if err != nil {
		return err
	}
	var next config.RuntimeSettings
	if err := json.Unmarshal(merged, &next); err != nil {
		return fmt.Errorf("decode runtime settings: %w", err)
	}
	if err := next.Validate(); err != nil {
		return err
	}
	host.manager.cfg.SetRuntimeSettings(next)
	host.manager.drive.SetTransferConcurrency(next.UploadConcurrency, next.DownloadConcurrency)
	return persistRuntimeSettings(ctx, host.manager.db, next)
}

func persistRuntimeSettings(ctx context.Context, db *database.DB, settings config.RuntimeSettings) error {
	values := map[string]string{
		config.SettingTGAppID:             strconv.Itoa(settings.AppID),
		config.SettingTGAppHash:           settings.AppHash,
		config.SettingLocalRoot:           settings.LocalRoot,
		config.SettingSegmentSize:         strconv.FormatInt(settings.SegmentSize, 10),
		config.SettingTGPoolSize:          strconv.FormatInt(settings.PoolSize, 10),
		config.SettingUploadThreads:       strconv.Itoa(settings.UploadThreads),
		config.SettingTGUploadPartSize:    strconv.FormatInt(settings.UploadPartSize, 10),
		config.SettingTGRateLimit:         settings.RateLimit.String(),
		config.SettingStreamConcurrency:   strconv.Itoa(settings.StreamConcurrency),
		config.SettingUploadConcurrency:   strconv.Itoa(settings.UploadConcurrency),
		config.SettingDownloadConcurrency: strconv.Itoa(settings.DownloadConcurrency),
		config.SettingWebDAVEnabled:       strconv.FormatBool(settings.WebDAVEnabled),
		config.SettingLogLevel:            settings.LogLevel,
		config.SettingCacheDir:            settings.CacheDir,
		config.SettingCacheLimit:          strconv.FormatInt(settings.CacheLimit, 10),
		config.SettingCacheTTL:            settings.CacheTTL.String(),
		config.SettingMaxDownloadConns:    strconv.Itoa(settings.MaxDownloadConns),
		config.SettingDownloadGrace:       settings.DownloadGrace.String(),
		config.SettingShareTTL:            settings.ShareTTL.String(),
	}
	return db.SetSettings(ctx, values)
}

func validatePluginDataKey(key string) error {
	if key == "" || len(key) > 128 || strings.ContainsAny(key, "\\/\x00") {
		return errors.New("plugin data key is invalid")
	}
	return nil
}

func decodeHostRequest(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode host request: %w", err)
	}
	return nil
}

func encodeHostResponse(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func publicEntry(entry drive.Entry) tdriveplugin.Entry {
	return tdriveplugin.Entry{
		Name: entry.Name, Path: entry.Path, IsDir: entry.IsDir, Size: entry.Size,
		MIME: entry.MIME, ID: entry.ID, SegmentCount: entry.SegmentCount,
		Status: entry.Status, ModifiedAt: time.UnixMilli(entry.ModifiedAt), CreatedAt: time.UnixMilli(entry.CreatedAt),
	}
}

func publicFile(file database.File) tdriveplugin.File {
	return tdriveplugin.File{
		ID: file.ID, DirID: file.DirID, Name: file.Name, Size: file.Size, MIME: file.MIME,
		SegmentCount: file.SegmentCount, Status: string(file.Status), CreatedAt: file.CreatedAt, UpdatedAt: file.UpdatedAt,
		OwnerID: file.OwnerID,
	}
}

func publicUploadJob(job database.UploadJob) tdriveplugin.UploadJob {
	return tdriveplugin.UploadJob{
		ID: job.ID, FileID: job.FileID, Name: job.Name, TotalSize: job.TotalSize,
		SegmentSize: job.SegmentSize, SegmentCount: job.SegmentCount,
		UploadedBytes: job.UploadedBytes, Status: string(job.Status), Error: job.Error,
	}
}

func publicDownloadJob(job database.DownloadJob) tdriveplugin.DownloadJob {
	return tdriveplugin.DownloadJob{
		ID: job.ID, FileID: job.FileID, Name: job.Name, TotalSize: job.TotalSize,
		DownloadedBytes: job.DownloadedBytes, Mode: string(job.Mode), Status: string(job.Status),
		Error: job.Error, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
}
