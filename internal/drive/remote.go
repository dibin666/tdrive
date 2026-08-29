package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
)

// RemoteRequest asks the server to fetch a URL straight into the drive, so a
// large file never has to travel through the user's browser.
type RemoteRequest struct {
	URL       string
	DirPath   string
	Name      string
	UserID    string
	Overwrite bool
}

// MaxRemoteRedirects matches the reference implementation's limit.
const MaxRemoteRedirects = 10

// RemoteProgress is notified as a server-side fetch advances.
type RemoteProgress func(job database.UploadJob, uploaded, total int64, err error)

// StartRemote begins a server-side fetch and returns as soon as the job exists.
//
// The transfer runs detached: a multi-gigabyte download must not hold an HTTP
// request open, and the job row plus SSE progress give the browser everything
// it needs to follow along or resume after a restart.
func (s *Service) StartRemote(ctx context.Context, req RemoteRequest) (database.UploadJob, error) {
	target, err := s.validateRemoteURL(req.URL)
	if err != nil {
		return database.UploadJob{}, err
	}

	size, name, contentType, err := s.probeRemote(ctx, target)
	if err != nil {
		return database.UploadJob{}, err
	}
	if req.Name != "" {
		name = req.Name
	}
	if err := ValidateName(name); err != nil {
		return database.UploadJob{}, err
	}
	if size < 0 {
		return database.UploadJob{}, errors.New(
			"the server hosting that URL does not report a file size, which tdrive needs before it can store the file")
	}

	job, _, err := s.Begin(ctx, UploadRequest{
		DirPath:   req.DirPath,
		Name:      name,
		Size:      size,
		MIME:      contentType,
		UserID:    req.UserID,
		Source:    "remote",
		SourceURL: target.String(),
		Overwrite: req.Overwrite,
	})
	if err != nil {
		return database.UploadJob{}, err
	}

	go s.runRemote(context.WithoutCancel(ctx), job, target)
	return job, nil
}

// ResumeRemotes picks up server-side fetches interrupted by a restart. Browser
// uploads cannot be resumed this way because only the browser holds the bytes.
func (s *Service) ResumeRemotes(ctx context.Context) {
	jobs, err := s.db.ResumableJobs(ctx)
	if err != nil {
		s.log.Warn("could not list resumable transfers", zap.Error(err))
		return
	}
	for _, job := range jobs {
		if job.Source == "local" {
			if job.SourceURL == "" {
				_ = s.db.SetJobStatus(ctx, job.ID, database.JobFailed, "local source path is missing")
				continue
			}
			s.log.Info("resuming a local transfer",
				zap.String("job", job.ID), zap.String("name", job.Name),
				zap.Ints("segments", job.PendingSegments()))
			go s.runLocal(context.WithoutCancel(ctx), job, job.SourceURL)
			continue
		}
		target, err := url.Parse(job.SourceURL)
		if err != nil {
			_ = s.db.SetJobStatus(ctx, job.ID, database.JobFailed, "stored source URL is invalid")
			continue
		}
		s.log.Info("resuming a remote transfer",
			zap.String("job", job.ID), zap.String("name", job.Name),
			zap.Ints("segments", job.PendingSegments()))
		go s.runRemote(context.WithoutCancel(ctx), job, target)
	}
}

// runRemote fetches the segments a job still needs.
//
// Each missing segment is requested with its own Range, so a resumed transfer
// re-downloads only what it lost rather than starting from zero. That is the
// same granularity the browser uploads at, which keeps one recovery story for
// both paths.
func (s *Service) runRemote(ctx context.Context, job database.UploadJob, target *url.URL) {
	if err := s.db.SetJobStatus(ctx, job.ID, database.JobRunning, ""); err != nil {
		s.log.Warn("could not mark a transfer running", zap.String("job", job.ID), zap.Error(err))
	}

	file, err := s.db.FileByID(ctx, job.FileID)
	if err != nil {
		s.failRemote(ctx, job, err)
		return
	}

	for _, index := range job.PendingSegments() {
		start := int64(index-1) * file.SegmentSize
		size := SegmentSize(file.Size, file.SegmentSize, index)

		body, err := s.fetchRange(ctx, target, start, size)
		if err != nil {
			s.failRemote(ctx, job, err)
			return
		}

		err = s.PutSegment(ctx, job, index, body, size, func(uploaded, total int64) {
			s.notifyRemote(job, file, index, start+uploaded, total, nil)
		})
		body.Close()
		if err != nil {
			s.failRemote(ctx, job, err)
			return
		}
	}

	if _, err := s.Complete(ctx, job.ID); err != nil {
		s.failRemote(ctx, job, err)
		return
	}
	completed := job
	completed.Status = database.JobComplete
	s.notifyRemote(completed, file, file.SegmentCount, file.Size, file.Size, nil)
}

func (s *Service) failRemote(ctx context.Context, job database.UploadJob, err error) {
	s.log.Warn("a remote transfer failed",
		zap.String("job", job.ID), zap.String("name", job.Name), zap.Error(err))
	if abortErr := s.Abort(ctx, job.ID, err.Error(), database.JobFailed); abortErr != nil {
		s.log.Warn("could not clean up a failed transfer",
			zap.String("job", job.ID), zap.Error(abortErr))
	}
	s.notifyRemote(job, database.File{Name: job.Name}, 0, 0, job.TotalSize, err)
}

func (s *Service) notifyRemote(job database.UploadJob, file database.File, index int, uploaded, total int64, err error) {
	if s.OnRemoteProgress == nil {
		return
	}
	s.OnRemoteProgress(job, uploaded, total, err)
}

// fetchRange downloads one byte range, following redirects itself so the hop
// count can be capped the way the reference implementation caps it.
func (s *Service) fetchRange(ctx context.Context, target *url.URL, start, size int64) (io.ReadCloser, error) {
	client := &http.Client{
		Timeout: 0, // a segment can legitimately take a long time
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxRemoteRedirects {
				return fmt.Errorf("stopped after %d redirects", MaxRemoteRedirects)
			}
			// Each hop is re-validated, so a redirect cannot be used to
			// reach an address the original URL was not allowed to.
			if _, err := s.validateRemoteURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tdrive/"+remoteUserAgentVersion)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+size-1))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", target.Redacted(), err)
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusOK:
		// The server ignored the range and is sending the whole file.
		// Skipping forward keeps the transfer correct at the cost of the
		// bytes already sent, which is better than failing outright.
		if start > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, start); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("skip to offset %d: %w", start, err)
			}
		}
		return newLimitedCloser(resp.Body, size), nil
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("fetch %s: server answered %s", target.Redacted(), resp.Status)
	}
}

// probeRemote asks for the size, name and type before anything is stored, so a
// URL that cannot work fails immediately rather than halfway through.
func (s *Service) probeRemote(ctx context.Context, target *url.URL) (size int64, name, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set("User-Agent", "tdrive/"+remoteUserAgentVersion)

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxRemoteRedirects {
				return fmt.Errorf("stopped after %d redirects", MaxRemoteRedirects)
			}
			if _, err := s.validateRemoteURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", fmt.Errorf("probe %s: %w", target.Redacted(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", "", fmt.Errorf("probe %s: server answered %s", target.Redacted(), resp.Status)
	}

	size = resp.ContentLength
	contentType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	name = remoteFilename(resp, target)
	return size, name, contentType, nil
}

// remoteFilename prefers the server's own Content-Disposition, falling back to
// the last path element.
func remoteFilename(resp *http.Response, target *url.URL) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if n := params["filename"]; n != "" {
				return path.Base(n)
			}
		}
	}
	if base := path.Base(target.Path); base != "" && base != "/" && base != "." {
		if decoded, err := url.PathUnescape(base); err == nil {
			return decoded
		}
		return base
	}
	return "download"
}

// validateRemoteURL restricts server-side fetches to public HTTP endpoints.
//
// Without this, "fetch a URL for me" is a request forgery primitive: anyone
// with a drive account could reach the container's own metadata service or
// anything else on the private network the drive happens to sit in.
func (s *Service) validateRemoteURL(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("that URL could not be parsed: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, errors.New("only http and https URLs can be fetched")
	}
	host := target.Hostname()
	if host == "" {
		return nil, errors.New("that URL has no host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("%s resolves to a private address, which tdrive will not fetch from", host)
		}
	}
	return target, nil
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		// 100.64.0.0/10, the carrier-grade NAT range cloud metadata services
		// and container networks sometimes sit in.
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1] >= 64 && ip.To4()[1] <= 127)
}

func newLimitedCloser(rc io.ReadCloser, n int64) io.ReadCloser {
	return &limitedCloser{Reader: io.LimitReader(rc, n), closer: rc}
}

type limitedCloser struct {
	io.Reader
	closer io.Closer
}

func (l *limitedCloser) Close() error { return l.closer.Close() }

const remoteUserAgentVersion = "1.0"
