package drive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

// WebDAV is where segment transparency has to hold for real clients. rclone,
// Finder and Windows Explorer all go through webdav.Handler, so exercising the
// handler directly covers what they will do: PROPFIND a listing, GET with a
// Range, PUT a body, MOVE, DELETE.
//
// This file lives in package drive rather than package dav so it can reuse the
// fake Telegram harness. The adapter it stands in for is thin; what is being
// tested is that a split file survives the round trip.

// davFS mirrors internal/dav's FileSystem against the test harness. Keeping it
// here rather than importing internal/dav avoids an import cycle through the
// auth package, and the logic under test — Stat sizes, Read stitching, PUT
// splitting — is the drive's, not the adapter's.
type davFS struct{ h *harness }

func (f *davFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	clean, err := CleanPath(name)
	if err != nil {
		return err
	}
	if _, err := f.h.svc.Stat(ctx, clean); err == nil {
		return os.ErrExist
	}
	_, err = f.h.svc.Mkdir(ctx, clean)
	return err
}

func (f *davFS) RemoveAll(ctx context.Context, name string) error {
	clean, err := CleanPath(name)
	if err != nil {
		return err
	}
	if err := f.h.svc.Delete(ctx, clean); err != nil {
		if strings.Contains(err.Error(), "no such file") {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

func (f *davFS) Rename(ctx context.Context, oldName, newName string) error {
	from, err := CleanPath(oldName)
	if err != nil {
		return err
	}
	to, err := CleanPath(newName)
	if err != nil {
		return err
	}
	fromDir, fromBase := Parent(from)
	toDir, toBase := Parent(to)

	if fromDir != toDir {
		if _, err := f.h.svc.Move(ctx, from, toDir); err != nil {
			return err
		}
		from = Join(toDir, fromBase)
	}
	if fromBase != toBase {
		if _, err := f.h.svc.Rename(ctx, from, toBase); err != nil {
			return err
		}
	}
	return nil
}

func (f *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return nil, err
	}
	entry, err := f.h.svc.Stat(ctx, clean)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return &davInfo{entry: entry}, nil
}

func (f *davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return nil, err
	}

	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		dir, base := Parent(clean)
		return &davWriter{h: f.h, ctx: ctx, dir: dir, name: base}, nil
	}

	entry, err := f.h.svc.Stat(ctx, clean)
	if err != nil {
		return nil, os.ErrNotExist
	}
	if entry.IsDir {
		children, err := f.h.svc.List(ctx, clean)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(children))
		for _, c := range children {
			infos = append(infos, &davInfo{entry: c})
		}
		return &davDir{info: davInfo{entry: entry}, children: infos}, nil
	}

	file, err := f.h.db.FileByID(ctx, entry.ID)
	if err != nil {
		return nil, err
	}
	stream, err := f.h.svc.OpenFile(ctx, file)
	if err != nil {
		return nil, err
	}
	return &davReader{ReadSeekCloser: stream, info: davInfo{entry: entry}}, nil
}

type davInfo struct{ entry Entry }

func (i *davInfo) Name() string { return i.entry.Name }
func (i *davInfo) Size() int64  { return i.entry.Size }
func (i *davInfo) Mode() os.FileMode {
	if i.entry.IsDir {
		return os.ModeDir | 0o555
	}
	return 0o444
}
func (i *davInfo) ModTime() time.Time { return time.UnixMilli(i.entry.ModifiedAt) }
func (i *davInfo) IsDir() bool        { return i.entry.IsDir }
func (i *davInfo) Sys() any           { return nil }

type davDir struct {
	info     davInfo
	children []os.FileInfo
	offset   int
}

func (d *davDir) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		rest := d.children[d.offset:]
		d.offset = len(d.children)
		return rest, nil
	}
	if d.offset >= len(d.children) {
		return nil, io.EOF
	}
	end := min(d.offset+count, len(d.children))
	page := d.children[d.offset:end]
	d.offset = end
	return page, nil
}
func (d *davDir) Stat() (os.FileInfo, error)     { return &d.info, nil }
func (d *davDir) Close() error                   { return nil }
func (d *davDir) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (d *davDir) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (d *davDir) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }

type davReader struct {
	io.ReadSeekCloser
	info davInfo
}

func (r *davReader) Stat() (os.FileInfo, error)         { return &r.info, nil }
func (r *davReader) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (r *davReader) Write([]byte) (int, error)          { return 0, os.ErrPermission }

type davWriter struct {
	h    *harness
	ctx  context.Context
	dir  string
	name string
	buf  bytes.Buffer
}

func (w *davWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *davWriter) Close() error {
	_, err := w.h.svc.UploadStream(w.ctx, UploadRequest{
		DirPath:   w.dir,
		Name:      w.name,
		Size:      int64(w.buf.Len()),
		Overwrite: true,
	}, bytes.NewReader(w.buf.Bytes()), nil)
	return err
}

func (w *davWriter) Stat() (os.FileInfo, error) {
	return &davInfo{entry: Entry{Name: w.name, Size: int64(w.buf.Len())}}, nil
}
func (w *davWriter) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (w *davWriter) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (w *davWriter) Seek(int64, int) (int64, error)     { return 0, os.ErrInvalid }

func newDAVServer(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	handler := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: &davFS{h: h},
		LockSystem: webdav.NewMemLS(),
		Logger: func(_ *http.Request, err error) {
			if err != nil && !os.IsNotExist(err) {
				t.Logf("webdav: %v", err)
			}
		},
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// The core acceptance criterion: a file stored across several Telegram objects
// must appear in a listing as one resource of the full size.
func TestWebDAVHidesSegmentation(t *testing.T) {
	h := newHarness(t, 4096)
	srv := newDAVServer(t, h)

	data := randomBytes(20_000, 61)
	h.store(t, "/", "split.bin", data)

	req, _ := http.NewRequest("PROPFIND", srv.URL+"/dav/", nil)
	req.Header.Set("Depth", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PROPFIND: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND returned %s", res.Status)
	}
	body, _ := io.ReadAll(res.Body)
	xml := string(body)

	// One resource, one size, no trace of the parts. Counting hrefs rather
	// than name occurrences, since a single response legitimately repeats the
	// name in both href and displayname.
	if n := strings.Count(xml, "<D:href>/dav/split.bin</D:href>"); n != 1 {
		t.Errorf("the file is listed as %d resources, want exactly 1:\n%s", n, xml)
	}
	if n := strings.Count(xml, "<D:response>"); n != 2 {
		t.Errorf("listing has %d responses, want 2 (the collection and the one file):\n%s", n, xml)
	}
	if strings.Contains(xml, ".part") {
		t.Errorf("segment names leaked into the listing:\n%s", xml)
	}
	if want := fmt.Sprintf("<D:getcontentlength>%d</D:getcontentlength>", len(data)); !strings.Contains(xml, want) {
		t.Errorf("listing does not report the logical size %d:\n%s", len(data), xml)
	}
}

// A full GET must reproduce the original bytes exactly. This is the equivalent
// of the rclone copy-and-compare acceptance check.
func TestWebDAVGetReassemblesFile(t *testing.T) {
	h := newHarness(t, 4096)
	srv := newDAVServer(t, h)

	data := randomBytes(50_000, 67)
	h.store(t, "/media", "movie.mkv", data)

	res, err := http.Get(srv.URL + "/dav/media/movie.mkv")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET returned %s", res.Status)
	}
	if res.ContentLength != int64(len(data)) {
		t.Errorf("Content-Length is %d, want %d", res.ContentLength, len(data))
	}

	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("body does not match the original: got %d bytes, want %d", len(got), len(data))
	}
}

// Range requests are what a mounted filesystem does constantly. They have to
// work across segment boundaries.
func TestWebDAVRangeRequests(t *testing.T) {
	const segSize = 4096
	h := newHarness(t, segSize)
	srv := newDAVServer(t, h)

	data := randomBytes(30_000, 71)
	h.store(t, "/", "ranged.bin", data)

	cases := []struct{ start, end int64 }{
		{0, 99},
		{4090, 4105},       // straddles the first boundary
		{segSize, segSize}, // exactly one byte at a boundary
		{8000, 17000},      // spans two boundaries
		{29_000, int64(len(data)) - 1},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d-%d", tc.start, tc.end), func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+"/dav/ranged.bin", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", tc.start, tc.end))

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusPartialContent {
				t.Fatalf("range request returned %s, want 206", res.Status)
			}
			got, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if want := data[tc.start : tc.end+1]; !bytes.Equal(got, want) {
				t.Errorf("range [%d,%d] does not match the original", tc.start, tc.end)
			}
		})
	}
}

// A PUT larger than one segment has to split on the way in and read back whole.
func TestWebDAVPutSplitsAndRoundTrips(t *testing.T) {
	h := newHarness(t, 4096)
	srv := newDAVServer(t, h)

	data := randomBytes(25_000, 73)
	req, _ := http.NewRequest("PUT", srv.URL+"/dav/uploaded.bin", bytes.NewReader(data))
	req.ContentLength = int64(len(data))

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT returned %s", res.Status)
	}

	stored, err := h.svc.Stat(context.Background(), "/uploaded.bin")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stored.SegmentCount < 2 {
		t.Errorf("a %d byte file was stored in %d segments; it should have split",
			len(data), stored.SegmentCount)
	}
	if stored.Size != int64(len(data)) {
		t.Errorf("stored size %d, want %d", stored.Size, len(data))
	}

	get, err := http.Get(srv.URL + "/dav/uploaded.bin")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer get.Body.Close()
	got, _ := io.ReadAll(get.Body)
	if !bytes.Equal(got, data) {
		t.Fatal("the file did not survive the PUT/GET round trip")
	}
}

func TestWebDAVMkcolMoveDelete(t *testing.T) {
	h := newHarness(t, 4096)
	srv := newDAVServer(t, h)
	client := &http.Client{}

	do := func(method, path string, headers map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		res.Body.Close()
		return res
	}

	if res := do("MKCOL", "/dav/albums", nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL returned %s", res.Status)
	}
	// A second MKCOL on the same collection must be refused.
	if res := do("MKCOL", "/dav/albums", nil); res.StatusCode == http.StatusCreated {
		t.Error("MKCOL on an existing collection succeeded")
	}

	h.store(t, "/albums", "song.flac", randomBytes(9_000, 79))

	if res := do("MOVE", "/dav/albums/song.flac", map[string]string{
		"Destination": srv.URL + "/dav/albums/renamed.flac",
	}); res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE returned %s", res.Status)
	}
	if _, err := h.svc.Stat(context.Background(), "/albums/renamed.flac"); err != nil {
		t.Errorf("file is not at its new path: %v", err)
	}

	if res := do("DELETE", "/dav/albums", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE returned %s", res.Status)
	}
	if _, err := h.svc.Stat(context.Background(), "/albums"); err == nil {
		t.Error("the collection still exists after DELETE")
	}
}

func TestWebDAVMissingFileIs404(t *testing.T) {
	h := newHarness(t, 4096)
	srv := newDAVServer(t, h)

	res, err := http.Get(srv.URL + "/dav/nope.bin")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing file returned %s, want 404", res.Status)
	}
}
