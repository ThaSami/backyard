// Package fs implements blob.Store on a local directory tree. It is the "no
// bucket" storage option and the target of `backyard deploy --method rsync`,
// and also works on top of mounted buckets (gcsfuse, blobfuse2; see below
// for mountpoint-s3).
//
// Keys map to files below the root directory ("sites/foo/index.html" →
// <root>/sites/foo/index.html). All access goes through [os.Root] handles,
// so no operation can reach outside the root. Beyond that, every directory
// on a key's path is opened one component at a time and must be a real
// directory (verified against its Lstat result); files are then created,
// renamed, read and removed by name relative to that directory handle. A
// concurrent writer below the root (e.g. a racing rsync) that swaps a
// directory for a symlink therefore cannot redirect a write, read or delete
// to another part of the tree (such as a database file kept in the root).
//
// Writes are atomic: data is written to a hidden temporary file in the
// destination directory and renamed into place. Path segments starting with
// ".backyard-tmp-" are reserved for these files: such keys are rejected with
// blob.ErrInvalidKey and such entries are hidden from List.
//
// Because files may be written by tools other than Backyard (rsync), object
// metadata is derived from the file itself:
//
//   - ETag is a hash of size and modification time (nanoseconds), so it
//     changes whenever rsync replaces a file. Put sets a fresh, unique mtime.
//   - Content type comes from blob.DetectContentType (extension, else
//     sniffing); the contentType argument of Put is not persisted.
//
// Symbolic links (rsync -a preserves them) are handled conservatively: a
// symlink is only followed when it is the last path component and its target
// is a relative path without ".." segments (so it resolves within the link's
// own directory subtree); chains are followed up to 8 links. Symlinked
// directories, absolute targets and targets containing ".." are never
// followed; such entries are invisible to Get/Stat/List. Directories reached
// through symlinks are never written to, listed or deleted from.
//
// Writes need rename support, so the fs store cannot write on mountpoint-s3;
// use the s3 backend there.
//
// The fs store cannot presign URLs; PresignPut and PresignGet return
// blob.ErrNotSupported (the deploy gateway serves self-hosted upload URLs
// instead).
package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/internal/blobutil"
)

const (
	// tmpPrefix marks in-flight writes; such files are hidden from List and
	// keys using it are rejected.
	tmpPrefix = ".backyard-tmp-"
	// staleTempAge is how old a temp file must be before DeletePrefix
	// considers it abandoned (left behind by a crash) and removes it.
	staleTempAge = time.Hour
	// maxSymlinks bounds symlink chains.
	maxSymlinks = 8
	// maxNameLen is the longest file name (path segment) most filesystems
	// accept, in bytes.
	maxNameLen = 255
	// putRetries bounds retries when a concurrent prune (in another
	// process) removes a directory between creating it and creating the
	// temp file in it.
	putRetries = 5
	// openRetries bounds retries when an object is replaced between
	// resolving and opening it.
	openRetries = 3

	dirPerm  = 0o755
	filePerm = 0o644
)

var (
	// errNotObject marks paths that exist but are not regular files (or
	// safe symlinks to one); it maps to blob.ErrNotFound.
	errNotObject = errors.New("not a regular file")
	// errNotDir marks key directories that exist but are not real
	// directories (files, symlinks, or entries swapped while opening).
	errNotDir = errors.New("not a directory")
	// errUnsafeLink marks symlinks that are not followed.
	errUnsafeLink = errors.New("symlink not followed")
	// errLinkLoop marks symlink chains longer than maxSymlinks.
	errLinkLoop = errors.New("too many levels of symbolic links")
	errStop     = errors.New("stop")
)

// Option configures a Store.
type Option func(*Store)

// WithFsync makes Put fsync the file before renaming it into place and the
// directory afterwards (durable, slower). Default off.
func WithFsync(on bool) Option { return func(s *Store) { s.fsync = on } }

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) {
		if l != nil {
			s.log = l
		}
	}
}

// Store is a blob.Store backed by a local directory. It is safe for
// concurrent use.
type Store struct {
	dir   string
	root  *os.Root
	fsync bool
	log   *slog.Logger

	// dirMu keeps directory pruning (write lock, held only around rmdir)
	// from removing a directory between Put creating it and creating its
	// temp file there (read lock). Across processes, Put retries instead.
	dirMu sync.RWMutex

	closeOnce sync.Once
	closeErr  error
}

var _ blob.Store = (*Store)(nil)

// New opens (creating it if needed) the directory dir as a blob store.
func New(dir string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("blob/fs: root directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("blob/fs: resolve root %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("blob/fs: create root %q: %w", abs, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("blob/fs: open root %q: %w", abs, err)
	}
	s := &Store{dir: abs, root: root, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	s.log = s.log.With("component", "blob", "backend", "fs")
	return s, nil
}

// Dir returns the absolute root directory.
func (s *Store) Dir() string { return s.dir }

func isNotFound(err error) bool {
	return errors.Is(err, iofs.ErrNotExist) ||
		errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.ENAMETOOLONG) ||
		errors.Is(err, errNotObject) ||
		errors.Is(err, errNotDir) ||
		errors.Is(err, errUnsafeLink) ||
		errors.Is(err, errLinkLoop)
}

func mapErr(op, key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %q", blob.ErrNotFound, key)
	}
	return fmt.Errorf("blob/fs: %s %q: %w", op, key, err)
}

func isTemp(name string) bool { return strings.HasPrefix(name, tmpPrefix) }

// checkKey applies the shared key rules and rejects path segments that use
// the reserved temp-file prefix or exceed the usual file name limit
// (NAME_MAX), so such keys yield ErrInvalidKey rather than an I/O error.
func checkKey(key string) error {
	if err := blobutil.CheckKey(key); err != nil {
		return err
	}
	for seg := range strings.SplitSeq(key, "/") {
		if isTemp(seg) {
			return fmt.Errorf("%w: %q (path segments starting with %q are reserved)", blob.ErrInvalidKey, key, tmpPrefix)
		}
		if len(seg) > maxNameLen {
			return fmt.Errorf("%w: path segment longer than %d bytes in %q", blob.ErrInvalidKey, maxNameLen, key)
		}
	}
	return nil
}

// splitKey splits a slash-separated path into its directory ("." for the
// root) and final name.
func splitKey(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ".", p
	}
	return p[:i], p[i+1:]
}

func joinKey(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// openSubdir opens the entry name of parent as a directory handle. The entry
// must be a real directory: symlinks are refused, and the opened handle must
// be the very directory that Lstat reported (so an entry swapped for a
// symlink in between is detected). With create, a missing entry is created.
func openSubdir(parent *os.Root, name string, create bool) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	if create && errors.Is(err, iofs.ErrNotExist) {
		if err := parent.Mkdir(name, dirPerm); err != nil && !errors.Is(err, iofs.ErrExist) {
			return nil, err
		}
		fi, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errNotDir
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	got, err := sub.Stat(".")
	if err != nil {
		sub.Close()
		return nil, err
	}
	if !got.IsDir() || !os.SameFile(fi, got) {
		sub.Close()
		return nil, errNotDir
	}
	return sub, nil
}

// openDir returns a handle on the key directory dir (slash-separated, "."
// for the root), opening it one component at a time with openSubdir. With
// create, missing directories are created. The caller must close it.
func (s *Store) openDir(dir string, create bool) (*os.Root, error) {
	if dir == "." || dir == "" {
		return s.root.OpenRoot(".")
	}
	cur := s.root
	done := ""
	for seg := range strings.SplitSeq(dir, "/") {
		done = joinKey(done, seg)
		next, err := openSubdir(cur, seg, create)
		if cur != s.root {
			cur.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", done, err)
		}
		cur = next
	}
	return cur, nil
}

// readDirOf returns the entries of the directory r, sorted by name.
func readDirOf(r *os.Root) ([]iofs.DirEntry, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// resolve locates the regular file designated by key path p, following only
// safe final-component symlinks. It returns a handle on the directory
// holding the file (the caller must close it), the file's name in that
// directory and its Lstat info.
func (s *Store) resolve(p string) (*os.Root, string, iofs.FileInfo, error) {
	for depth := 0; ; depth++ {
		dir, name := splitKey(p)
		parent, err := s.openDir(dir, false)
		if err != nil {
			return nil, "", nil, err
		}
		fi, err := parent.Lstat(name)
		if err != nil {
			parent.Close()
			return nil, "", nil, err
		}
		mode := fi.Mode()
		switch {
		case mode.IsRegular():
			return parent, name, fi, nil
		case mode&iofs.ModeSymlink != 0:
			target, err := parent.Readlink(name)
			parent.Close()
			if err != nil {
				return nil, "", nil, err
			}
			if depth >= maxSymlinks {
				return nil, "", nil, errLinkLoop
			}
			next, ok := linkTarget(dir, target)
			if !ok {
				s.log.Debug("ignoring unsafe symlink", "path", p)
				return nil, "", nil, errUnsafeLink
			}
			p = next
		default:
			parent.Close()
			return nil, "", nil, errNotObject
		}
	}
}

// linkTarget resolves a symlink target relative to the link's directory. Only
// relative targets without ".." segments are accepted.
func linkTarget(dir, target string) (string, bool) {
	if target == "" || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return "", false
	}
	t := filepath.ToSlash(target)
	if strings.HasPrefix(t, "/") {
		return "", false
	}
	parts := make([]string, 0, 4)
	for seg := range strings.SplitSeq(t, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", false
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return "", false
	}
	joined := joinKey(dir, strings.Join(parts, "/"))
	if checkKey(joined) != nil {
		return "", false
	}
	return joined, true
}

// info builds ObjectInfo for key from the file's info. When the extension
// does not determine the content type, the first bytes are sniffed from f,
// or (f == nil) from the regular file name in dir.
func (s *Store) info(key string, fi iofs.FileInfo, dir *os.Root, name string, f *os.File) *blob.ObjectInfo {
	ct := blob.ContentTypeByExt(key)
	if ct == "" {
		var head []byte
		switch {
		case f != nil:
			head = readHead(f)
		case dir != nil:
			if of, err := dir.OpenFile(name, readFlags, 0); err == nil {
				if ofi, err := of.Stat(); err == nil && ofi.Mode().IsRegular() {
					head = readHead(of)
				}
				of.Close()
			}
		}
		ct = blob.DetectContentType(key, head)
	}
	mod := fi.ModTime()
	return &blob.ObjectInfo{
		Key:         key,
		Size:        fi.Size(),
		ETag:        blobutil.SyntheticETag(fi.Size(), mod),
		ContentType: ct,
		ModTime:     mod.UTC(),
	}
}

func readHead(f *os.File) []byte {
	buf := make([]byte, blobutil.SniffLen)
	n, _ := f.ReadAt(buf, 0)
	return buf[:n]
}

// Get opens an object for reading.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, *blob.ObjectInfo, error) {
	return s.open(ctx, key, 0, -1)
}

// GetRange reads length bytes from offset (length < 0 = to the end). The
// returned ObjectInfo describes the whole object. Ranges past the end yield
// an empty body.
func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := blobutil.CheckRange(offset); err != nil {
		return nil, nil, err
	}
	return s.open(ctx, key, offset, length)
}

func (s *Store) open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	for attempt := 0; ; attempt++ {
		dir, name, fi, err := s.resolve(key)
		if err != nil {
			return nil, nil, mapErr("get", key, err)
		}
		f, err := dir.OpenFile(name, readFlags, 0)
		dir.Close()
		if err != nil {
			if isNotFound(err) && attempt < openRetries {
				continue // replaced or removed concurrently
			}
			return nil, nil, mapErr("get", key, err)
		}
		ofi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, mapErr("get", key, err)
		}
		if !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
			f.Close()
			if attempt < openRetries {
				continue
			}
			return nil, nil, mapErr("get", key, errNotObject)
		}
		info := s.info(key, ofi, nil, "", f)
		if offset >= ofi.Size() || length == 0 {
			f.Close()
			return blobutil.EmptyBody(), info, nil
		}
		if offset > 0 {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				f.Close()
				return nil, nil, mapErr("get", key, err)
			}
		}
		return blobutil.LimitedReadCloser(f, length), info, nil
	}
}

// Stat returns object metadata or blob.ErrNotFound.
func (s *Store) Stat(ctx context.Context, key string) (*blob.ObjectInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, name, fi, err := s.resolve(key)
	if err != nil {
		return nil, mapErr("stat", key, err)
	}
	defer dir.Close()
	return s.info(key, fi, dir, name, nil), nil
}

// lastMTime makes the modification times assigned by Put unique and
// increasing within the process, so ETags differ even for back-to-back
// writes of equal size.
var lastMTime atomic.Int64

func nextMTime() time.Time {
	for {
		now := time.Now().UnixNano()
		last := lastMTime.Load()
		if now <= last {
			now = last + 1
		}
		if lastMTime.CompareAndSwap(last, now) {
			return time.Unix(0, now)
		}
	}
}

func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type headWriter struct{ buf []byte }

func (h *headWriter) Write(p []byte) (int, error) {
	if room := blobutil.SniffLen - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Put writes an object atomically. The contentType argument is not stored:
// the fs backend always derives content types from the key (and content).
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*blob.ObjectInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("blob/fs: put: nil reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, name := splitKey(key)
	var (
		parent *os.Root
		f      *os.File
		tmp    string
	)
	for attempt := 0; ; attempt++ {
		var err error
		parent, f, tmp, err = s.createTemp(dir)
		if err == nil {
			break
		}
		if errors.Is(err, iofs.ErrNotExist) && attempt < putRetries {
			continue // a directory was pruned concurrently (another process)
		}
		return nil, fmt.Errorf("blob/fs: put %q: %w", key, err)
	}
	defer parent.Close()
	committed := false
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
		if !committed {
			if err := parent.Remove(tmp); err != nil && !errors.Is(err, iofs.ErrNotExist) {
				s.log.Warn("removing temp file failed", "dir", dir, "name", tmp, "err", err)
			}
		}
	}()

	var head *headWriter
	var w io.Writer = f
	if blob.ContentTypeByExt(key) == "" {
		head = &headWriter{}
		w = io.MultiWriter(f, head)
	}
	body := blobutil.ContextReader(ctx, blobutil.ExactSize(r, size))
	if _, err := io.Copy(w, body); err != nil {
		return nil, fmt.Errorf("blob/fs: put %q: %w", key, err)
	}
	if s.fsync {
		if err := f.Sync(); err != nil {
			return nil, fmt.Errorf("blob/fs: put %q: sync: %w", key, err)
		}
	}
	closed = true
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("blob/fs: put %q: close: %w", key, err)
	}
	fi, err := s.stampMTime(parent, tmp, name)
	if err != nil {
		return nil, fmt.Errorf("blob/fs: put %q: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := parent.Rename(tmp, name); err != nil {
		return nil, fmt.Errorf("blob/fs: put %q: %w", key, err)
	}
	committed = true
	if s.fsync {
		s.syncDir(parent, dir)
	}
	var headBytes []byte
	if head != nil {
		headBytes = head.buf
	}
	mod := fi.ModTime()
	return &blob.ObjectInfo{
		Key:         key,
		Size:        fi.Size(),
		ETag:        blobutil.SyntheticETag(fi.Size(), mod),
		ContentType: blob.DetectContentType(key, headBytes),
		ModTime:     mod.UTC(),
	}, nil
}

// createTemp opens (creating it if needed) the key directory dir and creates
// a new temp file in it. The caller must close the returned directory.
func (s *Store) createTemp(dir string) (*os.Root, *os.File, string, error) {
	s.dirMu.RLock()
	defer s.dirMu.RUnlock()
	parent, err := s.openDir(dir, true)
	if err != nil {
		return nil, nil, "", err
	}
	tmp := tmpPrefix + randSuffix()
	f, err := parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		parent.Close()
		return nil, nil, "", err
	}
	return parent, f, tmp, nil
}

// stampMTime gives the temp file a fresh unique mtime and makes sure the
// resulting ETag differs from the object it replaces even on filesystems
// with coarse timestamps. Filesystems that cannot set times (some FUSE
// mounts) keep the mtime assigned by the kernel.
func (s *Store) stampMTime(dir *os.Root, tmp, name string) (iofs.FileInfo, error) {
	m := nextMTime()
	if err := dir.Chtimes(tmp, m, m); err != nil {
		s.log.Debug("setting mtime failed; using filesystem time", "name", tmp, "err", err)
		return dir.Lstat(tmp)
	}
	fi, err := dir.Lstat(tmp)
	if err != nil {
		return nil, err
	}
	old, err := dir.Lstat(name)
	if err != nil || !old.Mode().IsRegular() || old.Size() != fi.Size() || !old.ModTime().Equal(fi.ModTime()) {
		return fi, nil
	}
	for _, d := range []time.Duration{time.Microsecond, time.Second, 2 * time.Second} {
		m := old.ModTime().Add(d)
		if err := dir.Chtimes(tmp, m, m); err != nil {
			break
		}
		if fi, err = dir.Lstat(tmp); err != nil {
			return nil, err
		}
		if !fi.ModTime().Equal(old.ModTime()) {
			break
		}
	}
	return fi, nil
}

func (s *Store) syncDir(dir *os.Root, name string) {
	d, err := dir.Open(".")
	if err != nil {
		return
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		s.log.Debug("directory fsync failed", "dir", name, "err", err)
	}
}

// Delete removes an object; missing objects (and directories) are ignored.
// A symlink key removes the link, never its target.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, name := splitKey(key)
	parent, err := s.openDir(dir, false)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("blob/fs: delete %q: %w", key, err)
	}
	fi, err := parent.Lstat(name)
	if err == nil && !fi.IsDir() {
		err = parent.Remove(name)
	}
	parent.Close()
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("blob/fs: delete %q: %w", key, err)
	}
	s.pruneDirs(dir)
	return nil
}

// removeEmptyChild removes the entry name of parent if it is an empty
// directory (never following symlinks); it reports whether it did.
func (s *Store) removeEmptyChild(parent *os.Root, name string) bool {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	fi, err := parent.Lstat(name)
	if err != nil || !fi.IsDir() {
		return false
	}
	return parent.Remove(name) == nil
}

// pruneDirs removes dir and its ancestors (never the root) while they are
// empty directories.
func (s *Store) pruneDirs(dir string) {
	for dir != "." && dir != "" {
		pdir, name := splitKey(dir)
		parent, err := s.openDir(pdir, false)
		if err != nil {
			return
		}
		removed := s.removeEmptyChild(parent, name)
		parent.Close()
		if !removed {
			return // not empty, not a directory, or gone
		}
		dir = pdir
	}
}

// splitPrefix splits a listing prefix into the directory to read and the
// name prefix entries in it must have.
func splitPrefix(prefix string) (dir, name string) {
	i := strings.LastIndexByte(prefix, '/')
	if i < 0 {
		return ".", prefix
	}
	return prefix[:i], prefix[i+1:]
}

// entryInfo returns object info for the non-directory entry name of dir
// (whose key is key), following safe symlinks; ok is false for entries that
// are not objects.
func (s *Store) entryInfo(dir *os.Root, key, name string, e iofs.DirEntry) (*blob.ObjectInfo, bool) {
	if checkKey(key) != nil {
		return nil, false
	}
	t := e.Type()
	switch {
	case t.IsRegular():
		fi, err := dir.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, false
		}
		return s.info(key, fi, dir, name, nil), true
	case t&iofs.ModeSymlink != 0:
		rdir, rname, fi, err := s.resolve(key)
		if err != nil {
			if !isNotFound(err) {
				s.log.Debug("skipping symlink", "path", key, "err", err)
			}
			return nil, false
		}
		defer rdir.Close()
		return s.info(key, fi, rdir, rname, nil), true
	default:
		return nil, false
	}
}

// walk calls fn for every object below the directory handle r (whose key
// is dir), depth first.
func (s *Store) walk(ctx context.Context, r *os.Root, dir string, fn func(*blob.ObjectInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := readDirOf(r)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if isTemp(name) {
			continue
		}
		key := joinKey(dir, name)
		if e.IsDir() {
			if err := s.walkSubdir(ctx, r, name, key, fn); err != nil {
				return err
			}
			continue
		}
		if info, ok := s.entryInfo(r, key, name, e); ok {
			if err := fn(info); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkSubdir walks the subdirectory name of r (whose key is key).
func (s *Store) walkSubdir(ctx context.Context, r *os.Root, name, key string, fn func(*blob.ObjectInfo) error) error {
	if blobutil.CheckPrefix(key+"/") != nil {
		return nil
	}
	sub, err := openSubdir(r, name, false)
	if err != nil {
		if isNotFound(err) {
			return nil // removed or swapped concurrently
		}
		return err
	}
	defer sub.Close()
	return s.walk(ctx, sub, key, fn)
}

// List returns objects under prefix sorted by key. With delimiter "/",
// sub-directories that contain at least one object are returned as Prefixes.
func (s *Store) List(ctx context.Context, prefix, delimiter string) (*blob.ListResult, error) {
	if err := blobutil.CheckPrefix(prefix); err != nil {
		return nil, err
	}
	if err := blobutil.CheckDelimiter(delimiter); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res := &blob.ListResult{}
	baseDir, namePrefix := splitPrefix(prefix)
	base, err := s.openDir(baseDir, false)
	if err != nil {
		if isNotFound(err) {
			return res, nil
		}
		return nil, fmt.Errorf("blob/fs: list %q: %w", prefix, err)
	}
	defer base.Close()
	entries, err := readDirOf(base)
	if err != nil {
		if isNotFound(err) {
			return res, nil
		}
		return nil, fmt.Errorf("blob/fs: list %q: %w", prefix, err)
	}
	add := func(info *blob.ObjectInfo) error {
		res.Objects = append(res.Objects, *info)
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, namePrefix) || isTemp(name) {
			continue
		}
		key := joinKey(baseDir, name)
		if e.IsDir() {
			if delimiter == "/" {
				err := s.walkSubdir(ctx, base, name, key, func(*blob.ObjectInfo) error { return errStop })
				switch {
				case errors.Is(err, errStop):
					res.Prefixes = append(res.Prefixes, key+"/")
				case err != nil:
					return nil, fmt.Errorf("blob/fs: list %q: %w", prefix, err)
				}
				continue
			}
			if err := s.walkSubdir(ctx, base, name, key, add); err != nil {
				return nil, fmt.Errorf("blob/fs: list %q: %w", prefix, err)
			}
			continue
		}
		if info, ok := s.entryInfo(base, key, name, e); ok {
			res.Objects = append(res.Objects, *info)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	blobutil.SortListResult(res)
	return res, nil
}

// DeletePrefix removes every object whose key starts with prefix and prunes
// directories left empty. Symlinks are removed, never followed. An empty
// prefix is rejected.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if err := blobutil.CheckDeletePrefix(prefix); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	baseDir, namePrefix := splitPrefix(prefix)
	base, err := s.openDir(baseDir, false)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("blob/fs: delete prefix %q: %w", prefix, err)
	}
	firstErr := s.removeEntries(ctx, base, func(name string) bool { return strings.HasPrefix(name, namePrefix) })
	base.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.pruneDirs(baseDir)
	if firstErr != nil {
		return fmt.Errorf("blob/fs: delete prefix %q: %w", prefix, firstErr)
	}
	return nil
}

// removeEntries deletes the entries of dir accepted by match (recursively
// for directories, which are removed once empty) without following
// symlinks. In-flight temp files of concurrent Puts are left alone; stale
// ones are removed. It returns the first error.
func (s *Store) removeEntries(ctx context.Context, dir *os.Root, match func(string) bool) error {
	entries, err := readDirOf(dir)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, e := range entries {
		name := e.Name()
		if !match(name) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() {
			keep(s.removeTree(ctx, dir, name))
			continue
		}
		if isTemp(name) && !stale(dir, name) {
			continue
		}
		if err := dir.Remove(name); err != nil && !isNotFound(err) {
			keep(err)
		}
	}
	return firstErr
}

// removeTree deletes everything below the subdirectory name of parent, then
// the subdirectory itself if it ended up empty (a concurrent Put may have
// added a file).
func (s *Store) removeTree(ctx context.Context, parent *os.Root, name string) error {
	sub, err := openSubdir(parent, name, false)
	if err != nil {
		if isNotFound(err) {
			return nil // gone, or swapped for a non-directory concurrently
		}
		return err
	}
	err = s.removeEntries(ctx, sub, func(string) bool { return true })
	sub.Close()
	s.removeEmptyChild(parent, name)
	return err
}

func stale(dir *os.Root, name string) bool {
	fi, err := dir.Lstat(name)
	return err == nil && time.Since(fi.ModTime()) > staleTempAge
}

// PresignPut is not supported by the fs backend.
func (s *Store) PresignPut(_ context.Context, key, _ string, _ time.Duration) (*blob.PresignedRequest, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: fs storage cannot presign uploads", blob.ErrNotSupported)
}

// PresignGet is not supported by the fs backend.
func (s *Store) PresignGet(_ context.Context, key string, _ time.Duration) (*blob.PresignedRequest, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: fs storage cannot presign downloads", blob.ErrNotSupported)
}

// Ping verifies the root directory is writable.
func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := tmpPrefix + "ping-" + randSuffix()
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("blob/fs: root %q is not writable: %w", s.dir, err)
	}
	f.Close()
	if err := s.root.Remove(name); err != nil {
		return fmt.Errorf("blob/fs: ping cleanup: %w", err)
	}
	return nil
}

// Describe returns {type: "fs"}; the local path is deliberately not exposed.
func (s *Store) Describe() blob.Description { return blob.Description{Type: "fs"} }

// Close releases the root directory handle. It is safe to call more than
// once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.root.Close() })
	return s.closeErr
}
