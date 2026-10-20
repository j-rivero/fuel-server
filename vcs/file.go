package vcs

import (
	"bytes"
	"context"
	"fmt"
	"github.com/gazebo-web/gz-go/v7"
	"github.com/pkg/errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// File is a file stored in a repository at a given revision. It only holds
// metadata: its content is not read until Content is called, so a File that
// is never read holds no resources.
type File struct {
	// Hash is the git object hash of the file content.
	Hash string
	// Size is the size of the file content, in bytes.
	Size int64
	// repoPath is the path of the repository that stores the file.
	repoPath string
}

// bigFileThreshold overrides git's core.bigFileThreshold (512 MiB by default)
// when the file content is extracted. git loads packed blobs smaller than the
// threshold in memory, while it streams larger ones, unless they are stored
// as deltas: those are always loaded in memory by git.
const bigFileThreshold = 1 << 20

// extractions limits the number of file contents extracted at the same time,
// and so the number of concurrent git processes and the disk bandwidth they
// use. Extracting a file waits while the limit is reached.
var extractions = make(chan struct{}, runtime.GOMAXPROCS(0))

// openFile returns the file at pathFromRoot in the given revision of the
// repository at repoPath. It fails if the path does not exist or is not a
// file (eg. a folder).
func openFile(ctx context.Context, repoPath, rev, pathFromRoot string) (*File, error) {
	if err := ensureFolderExists(repoPath); err != nil {
		return nil, err
	}
	// git cat-file --batch-check reads one object name per line.
	if strings.Contains(pathFromRoot, "\n") {
		return nil, errors.New("Invalid file path: " + pathFromRoot)
	}
	rev = ensureRev(rev)
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(rev + ":" + pathFromRoot + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	bs, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, gz.WithStack(ctx.Err())
		}
		err = gz.WithStack(err)
		gz.LoggerFromContext(ctx).Info("Error while running git cat-file process. Err: " + fmt.Sprint(err) + ". Stderr: " + stderr.String())
		return nil, err
	}
	// The output is "<hash> <type> <size>", or "<object> missing".
	fields := strings.Fields(string(bs))
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, errors.New("File not found: " + pathFromRoot)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return nil, errors.New("Invalid size of file " + pathFromRoot + ": " + fields[2])
	}
	return &File{Hash: fields[0], Size: size, repoPath: repoPath}, nil
}

// Content extracts the file content to a temporary file, and returns it
// opened at its beginning. The content is checked to be complete before
// returning, so that a file that cannot be read fails before a response is
// sent. The caller must close the content, which removes the temporary file.
// git is killed if ctx is done before the content is extracted.
//
// The temporary file is created in os.TempDir (see $TMPDIR), which needs room
// for the files being downloaded at the same time. If it is a tmpfs, the
// content is held in memory.
func (f *File) Content(ctx context.Context) (io.ReadSeekCloser, error) {
	select {
	case extractions <- struct{}{}:
	case <-ctx.Done():
		return nil, gz.WithStack(ctx.Err())
	}
	defer func() { <-extractions }()

	tmp, err := newTempFile()
	if err != nil {
		return nil, err
	}
	if err := f.extractTo(ctx, tmp.File); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	return tmp, nil
}

// extractTo writes the file content to dst with git, and seeks dst to its
// beginning.
func (f *File) extractTo(ctx context.Context, dst *os.File) error {
	threshold := fmt.Sprintf("core.bigFileThreshold=%d", bigFileThreshold)
	cmd := exec.CommandContext(ctx, "git", "-C", f.repoPath, "-c", threshold, "cat-file", "blob", f.Hash)
	// git writes to the file directly, the content is not copied by this
	// process.
	cmd.Stdout = dst
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// git was killed because the request went away.
			return gz.WithStack(ctx.Err())
		}
		err = errors.Wrap(err, "git cat-file failed. Stderr: "+strings.TrimSpace(stderr.String()))
		gz.LoggerFromContext(ctx).Info("Error while running git cat-file process. Err: " + fmt.Sprint(err))
		return err
	}
	info, err := dst.Stat()
	if err != nil {
		return gz.WithStack(err)
	}
	if info.Size() != f.Size {
		return errors.Errorf("vcs: extracted %d bytes of file %s, expected %d", info.Size(), f.Hash, f.Size)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return gz.WithStack(err)
	}
	return nil
}

// tempFile is a temporary file that is removed when closed.
type tempFile struct {
	*os.File
	// name is the path of the file, if it was not removed yet.
	name string
}

// newTempFile creates a temporary file. Where possible (eg. Linux), the file
// is removed right away, so that its disk space is released once it is closed
// even if the server stops.
func newTempFile() (*tempFile, error) {
	f, err := os.CreateTemp("", "fuel-file-*")
	if err != nil {
		return nil, gz.WithStack(err)
	}
	t := &tempFile{File: f, name: f.Name()}
	if os.Remove(t.name) == nil {
		t.name = ""
	}
	return t, nil
}

// Close closes and removes the file.
func (t *tempFile) Close() error {
	err := t.File.Close()
	if t.name != "" {
		if rerr := os.Remove(t.name); rerr != nil && err == nil {
			err = rerr
		}
		t.name = ""
	}
	return err
}
