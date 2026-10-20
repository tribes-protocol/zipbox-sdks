package zipbox

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path"
	"time"
)

// MaxFileBytes is the largest file one read or write moves (250 MiB).
const MaxFileBytes = 250 * 1024 * 1024

// FileKind is the type of a directory entry.
type FileKind string

// Directory entry kinds.
const (
	KindDirectory FileKind = "directory"
	KindFile      FileKind = "file"
	KindSymlink   FileKind = "symlink"
	KindOther     FileKind = "other"
)

// FileEntry is one directory entry.
type FileEntry struct {
	Name         string   `json:"name"`
	Kind         FileKind `json:"kind"`
	SizeBytes    int64    `json:"sizeBytes"`
	ModifiedAtMs int64    `json:"modifiedAtMs"`
}

// ModifiedAt is the entry's modification time.
func (e FileEntry) ModifiedAt() time.Time { return time.UnixMilli(e.ModifiedAtMs) }

// FilesService reads and writes a box's filesystem. Reach it as Box.Files.
// Paths are absolute, or relative to /root/workspace.
type FilesService struct {
	box *Box
}

// Read returns a file's bytes.
func (s *FilesService) Read(ctx context.Context, p string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.ReadTo(ctx, p, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadTo streams a file into w and returns the bytes copied.
func (s *FilesService) ReadTo(ctx context.Context, p string, w io.Writer) (int64, error) {
	resp, err := s.box.do(ctx, &request{
		method: "GET",
		query:  url.Values{"path": {ResolvePath(p)}},
		retry:  true,
	}, "/files/download")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// Write writes content to a file atomically, replacing it. The parent
// directory must exist (see Mkdir).
func (s *FilesService) Write(ctx context.Context, p string, content io.Reader) error {
	body, err := replayable(content)
	if err != nil {
		return err
	}
	resolved := ResolvePath(p)
	resp, err := s.box.do(ctx, &request{
		method:      "POST",
		query:       url.Values{"dir": {path.Dir(resolved)}, "name": {path.Base(resolved)}},
		body:        body,
		contentType: "application/octet-stream",
	}, "/files/write")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// replayable turns a reader into a body factory a retry can call again: a
// seeker is rewound, anything else is buffered (writes are capped at 250 MiB).
func replayable(r io.Reader) (func() (io.Reader, error), error) {
	if seeker, ok := r.(io.ReadSeeker); ok {
		start, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		return func() (io.Reader, error) {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				return nil, err
			}
			return io.NopCloser(seeker), nil
		}, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, newError(CodeInvalidRequest, "file is larger than 250 MiB")
	}
	return func() (io.Reader, error) { return bytes.NewReader(data), nil }, nil
}

// List returns a directory's entries.
func (s *FilesService) List(ctx context.Context, p string) ([]FileEntry, error) {
	var out struct {
		Entries []FileEntry `json:"entries"`
	}
	if err := s.box.json(ctx, "GET", "/files", url.Values{"path": {ResolvePath(p)}}, nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// Stat returns one entry, read from its parent directory's listing.
func (s *FilesService) Stat(ctx context.Context, p string) (*FileEntry, error) {
	resolved := ResolvePath(p)
	entries, err := s.List(ctx, path.Dir(resolved))
	if err != nil {
		return nil, err
	}
	name := path.Base(resolved)
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i], nil
		}
	}
	zerr := newError(CodeNotFound, "no such file: "+resolved)
	zerr.Status = 404
	return nil, zerr
}

type mutation struct {
	Action      string `json:"action"`
	Path        string `json:"path"`
	Destination string `json:"destination,omitempty"`
}

func (s *FilesService) mutate(ctx context.Context, m mutation) error {
	return s.box.json(ctx, "POST", "/files/mutate", nil, m, nil)
}

// Mkdir creates a directory.
func (s *FilesService) Mkdir(ctx context.Context, p string) error {
	return s.mutate(ctx, mutation{Action: "mkdir", Path: ResolvePath(p)})
}

// Move moves or renames src to dst.
func (s *FilesService) Move(ctx context.Context, src, dst string) error {
	return s.mutate(ctx, mutation{Action: "move", Path: ResolvePath(src), Destination: ResolvePath(dst)})
}

// Copy copies src to dst.
func (s *FilesService) Copy(ctx context.Context, src, dst string) error {
	return s.mutate(ctx, mutation{Action: "copy", Path: ResolvePath(src), Destination: ResolvePath(dst)})
}

// Delete deletes a file or directory.
func (s *FilesService) Delete(ctx context.Context, p string) error {
	return s.mutate(ctx, mutation{Action: "delete", Path: ResolvePath(p)})
}

// Upload copies a local file to the box.
func (s *FilesService) Upload(ctx context.Context, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Write(ctx, remotePath, f)
}

// Download copies a file from the box to localPath, replacing it.
func (s *FilesService) Download(ctx context.Context, remotePath, localPath string) error {
	tmp, err := os.CreateTemp(path.Dir(localPath), ".zipbox-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := s.ReadTo(ctx, remotePath, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), localPath)
}
