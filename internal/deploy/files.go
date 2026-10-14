package deploy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/thasami/backyard/internal/blob"
)

// LocalFile is a file selected for deployment.
type LocalFile struct {
	Path        string // site-relative, "/"-separated
	Abs         string // absolute path on disk
	Size        int64
	SHA256      string // hex
	ContentType string
}

// maxIgnoreFileBytes bounds .backyardignore.
const maxIgnoreFileBytes = 1 << 20

// utf8BOM is the UTF-8 encoding of U+FEFF, the byte order mark.
const utf8BOM = "\uFEFF"

// IgnorePatterns returns the complete pattern list CollectFiles applies to
// dir: WithDefaultIgnore of the lines of dir/.backyardignore (if present)
// followed by extra — so those may re-include the overridable defaults but
// never ProtectedIgnore. Direct-sync methods use it to build exclude flags.
func IgnorePatterns(dir string, extra []string) ([]string, error) {
	var patterns []string
	f, err := os.Open(filepath.Join(dir, IgnoreFile))
	switch {
	case err == nil:
		defer f.Close()
		sc := bufio.NewScanner(io.LimitReader(f, maxIgnoreFileBytes))
		sc.Buffer(make([]byte, 0, 4096), maxIgnoreFileBytes)
		for first := true; sc.Scan(); first = false {
			line := sc.Text()
			if first {
				// Editors on Windows (PowerShell 5's -Encoding utf8, older
				// Notepad) start the file with a UTF-8 byte order mark,
				// which git ignores too; kept, it would make the first rule
				// match nothing.
				line = strings.TrimPrefix(line, utf8BOM)
			}
			patterns = append(patterns, line)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("deploy: read %s: %w", IgnoreFile, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("deploy: read %s: %w", IgnoreFile, err)
	}
	return WithDefaultIgnore(append(patterns, extra...)), nil
}

// CollectFiles walks dir applying the default ignore rules,
// dir/.backyardignore and extra patterns (gitignore syntax, see
// IgnorePatterns), hashing every selected file. Symlinks to
// files are followed; symlinked directories are not.
//
// Symlinks whose target lies outside dir, broken symlinks and special files
// (sockets, devices, pipes) are skipped. A selected file whose path the
// gateway would reject (e.g. under "__backyard/") is an error. The result is
// sorted by Path.
func CollectFiles(dir string, extra []string) ([]LocalFile, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("deploy: %s is not a directory", dir)
	}
	patterns, err := IgnorePatterns(root, extra)
	if err != nil {
		return nil, err
	}
	m := NewMatcher(patterns)

	var files []LocalFile
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
		rel = filepath.ToSlash(rel)
		typ := d.Type()
		switch {
		case d.IsDir():
			if m.Ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		case typ&fs.ModeSymlink != 0:
			if m.Ignored(rel, false) || !symlinkedFileWithin(root, p) {
				return nil
			}
		case !typ.IsRegular():
			return nil
		default:
			if m.Ignored(rel, false) {
				return nil
			}
		}
		if why := ValidatePath(rel); why != "" {
			return fmt.Errorf("deploy: cannot deploy %q: %s (add it to %s to skip it)", rel, why, IgnoreFile)
		}
		files = append(files, LocalFile{Path: rel, Abs: p})
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := hashFiles(context.Background(), files); err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b LocalFile) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// symlinkedFileWithin reports whether the symlink at p resolves to a regular
// file inside root (symlinked directories are not followed).
func symlinkedFileWithin(root, p string) bool {
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false // broken
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false // escapes dir
	}
	st, err := os.Stat(target)
	return err == nil && st.Mode().IsRegular()
}

// hashFiles fills Size, SHA256 and ContentType in parallel.
func hashFiles(ctx context.Context, files []LocalFile) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	work := make(chan int)
	for range min(max(runtime.NumCPU(), 2), 8) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := hashFile(&files[i]); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for i := range files {
		select {
		case work <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return firstErr
}

// hashFile reads one file, recording its size, sha256 and content type.
func hashFile(f *LocalFile) error {
	fh, err := os.Open(f.Abs)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	defer fh.Close()
	h := sha256.New()
	head := make([]byte, 512)
	n, err := io.ReadFull(fh, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("deploy: read %s: %w", f.Path, err)
	}
	head = head[:n]
	h.Write(head)
	rest, err := io.Copy(h, fh)
	if err != nil {
		return fmt.Errorf("deploy: read %s: %w", f.Path, err)
	}
	f.Size = int64(n) + rest
	f.SHA256 = hex.EncodeToString(h.Sum(nil))
	f.ContentType = blob.DetectContentType(f.Path, head)
	return nil
}
