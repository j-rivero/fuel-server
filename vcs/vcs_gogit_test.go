package vcs

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRepo creates a git repository with the given files.
func newTestRepo(t *testing.T, files map[string][]byte) *GoGitVCS {
	dir := t.TempDir()
	writeFiles(t, dir, files)
	repo := GoGitVCS{}.NewRepo(dir).(*GoGitVCS)
	require.NoError(t, repo.InitRepo(context.Background()))
	return repo
}

func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, content, 0644))
	}
}

func randomBytes(t *testing.T, size int) []byte {
	bs := make([]byte, size)
	_, err := rand.Read(bs)
	require.NoError(t, err)
	return bs
}

// forLooseAndPacked runs check on repo with its objects loose, as they are
// written, and then packed.
func forLooseAndPacked(t *testing.T, repo *GoGitVCS, check func(t *testing.T, repo *GoGitVCS)) {
	t.Run("loose objects", func(t *testing.T) { check(t, repo) })

	out, err := exec.Command("git", "-C", repo.Path, "repack", "-a", "-d").CombinedOutput()
	require.NoError(t, err, string(out))
	// Reopen the repo so that go-git sees the new packfile.
	repo = GoGitVCS{}.NewRepo(repo.Path).(*GoGitVCS)
	t.Run("packed objects", func(t *testing.T) { check(t, repo) })
}

func readFile(t *testing.T, repo VCS, rev, path string) []byte {
	ctx := context.Background()
	f, err := repo.OpenFile(ctx, rev, path)
	require.NoError(t, err)
	c, err := f.Content(ctx)
	require.NoError(t, err)
	defer c.Close()
	bs, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, f.Size, int64(len(bs)))
	return bs
}

func TestOpenFile(t *testing.T) {
	small := []byte("<?xml version=\"1.0\"?>\n<model/>\n")
	large := randomBytes(t, bigFileThreshold+1)
	repo := newTestRepo(t, map[string][]byte{
		"model.config":     small,
		"meshes/large.dae": large,
	})
	ctx := context.Background()

	forLooseAndPacked(t, repo, func(t *testing.T, repo *GoGitVCS) {
		for _, r := range []VCS{repo, GitVCS{}.NewRepo(repo.Path)} {
			assert.Equal(t, small, readFile(t, r, "", "model.config"))
			assert.Equal(t, large, readFile(t, r, "tip", "meshes/large.dae"))

			// The hash is the blob hash.
			f, err := r.OpenFile(ctx, "", "meshes/large.dae")
			require.NoError(t, err)
			out, err := exec.Command("git", "-C", repo.Path, "rev-parse", "HEAD:meshes/large.dae").Output()
			require.NoError(t, err)
			assert.Equal(t, string(out[:len(out)-1]), f.Hash)
			assert.Equal(t, int64(len(large)), f.Size)

			for _, path := range []string{"meshes", ".", "", "missing.sdf", "meshes/missing.dae",
				"/model.config", "../model.config", "model.config\nHEAD:meshes/large.dae"} {
				_, err = r.OpenFile(ctx, "", path)
				assert.Error(t, err, path)
			}
			_, err = r.OpenFile(ctx, "missing-rev", "model.config")
			assert.Error(t, err)
		}
	})
}

func TestOpenFileRevisions(t *testing.T) {
	// The versions are similar, so that packing stores one as a delta of the
	// other.
	v1 := randomBytes(t, bigFileThreshold+1)
	v2 := append(append([]byte{}, v1...), []byte("v2")...)
	v2[1000] ^= 0xff
	repo := newTestRepo(t, map[string][]byte{"large.dae": v1, "model.sdf": []byte("v1")})
	ctx := context.Background()

	// ReplaceFiles commits with go-git, which writes loose objects.
	folder := t.TempDir()
	writeFiles(t, folder, map[string][]byte{"large.dae": v2, "model.sdf": []byte("v2")})
	require.NoError(t, repo.ReplaceFiles(ctx, folder, "owner"))

	forLooseAndPacked(t, repo, func(t *testing.T, repo *GoGitVCS) {
		assert.Equal(t, v2, readFile(t, repo, "", "large.dae"))
		assert.Equal(t, []byte("v2"), readFile(t, repo, "", "model.sdf"))
		assert.Equal(t, v1, readFile(t, repo, "HEAD~1", "large.dae"))
		assert.Equal(t, []byte("v1"), readFile(t, repo, "HEAD~1", "model.sdf"))
	})
}

func TestOpenFileUnreadable(t *testing.T) {
	tmp := useTempDir(t)
	data := randomBytes(t, 2*bigFileThreshold)
	repo := newTestRepo(t, map[string][]byte{"large.dae": data})
	ctx := context.Background()
	f, err := repo.OpenFile(ctx, "", "large.dae")
	require.NoError(t, err)
	objectPath := filepath.Join(repo.Path, ".git", "objects", f.Hash[:2], f.Hash[2:])

	// The object is replaced with a truncated one: its size can be read, but
	// not its content.
	var object bytes.Buffer
	zw := zlib.NewWriter(&object)
	_, err = fmt.Fprintf(zw, "blob %d\x00", len(data))
	require.NoError(t, err)
	_, err = zw.Write(data[:100])
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.Remove(objectPath))
	require.NoError(t, os.WriteFile(objectPath, object.Bytes(), 0444))
	f, err = repo.OpenFile(ctx, "", "large.dae")
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), f.Size)
	_, err = f.Content(ctx)
	require.Error(t, err)

	// The object goes away after the file was opened.
	require.NoError(t, os.Remove(objectPath))
	_, err = f.Content(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status")
	assertEmptyDir(t, tmp)
}

func TestOpenFileClosesFiles(t *testing.T) {
	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		t.Skip("the open files cannot be counted")
	}
	countFDs := func() int {
		fds, err := os.ReadDir("/proc/self/fd")
		require.NoError(t, err)
		return len(fds)
	}
	tmp := useTempDir(t)
	repo := newTestRepo(t, map[string][]byte{"model.sdf": []byte("<sdf/>")})
	// Files that are not closed are only closed by their finalizer.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	ctx := context.Background()

	forLooseAndPacked(t, repo, func(t *testing.T, repo *GoGitVCS) {
		before := countFDs()
		for i := 0; i < 100; i++ {
			// A file whose content is not requested holds no files.
			f, err := repo.OpenFile(ctx, "", "model.sdf")
			require.NoError(t, err)
			f, err = repo.OpenFile(ctx, "", "model.sdf")
			require.NoError(t, err)
			c, err := f.Content(ctx)
			require.NoError(t, err)
			require.NoError(t, c.Close())
			_, err = repo.OpenFile(ctx, "", "missing.sdf")
			require.Error(t, err)
		}
		assert.Less(t, countFDs()-before, 10)
		assertEmptyDir(t, tmp)
	})
}

// totalAlloc returns the bytes allocated while running fn.
func totalAlloc(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestLargeFileIsNotLoadedInMemory checks the memory allocated by this
// process. Note that it does not include the memory used by git.
func TestLargeFileIsNotLoadedInMemory(t *testing.T) {
	const size = 32 << 20
	repo := newTestRepo(t, map[string][]byte{"large.dae": randomBytes(t, size)})
	ctx := context.Background()

	forLooseAndPacked(t, repo, func(t *testing.T, repo *GoGitVCS) {
		alloc := totalAlloc(func() {
			f, err := repo.OpenFile(ctx, "", "large.dae")
			require.NoError(t, err)
			c, err := f.Content(ctx)
			require.NoError(t, err)
			defer c.Close()
			n, err := io.Copy(io.Discard, c)
			require.NoError(t, err)
			require.Equal(t, int64(size), n)
		})
		assert.Less(t, alloc, uint64(size/8), "OpenFile allocated %d bytes", alloc)

		alloc = totalAlloc(func() {
			require.NoError(t, repo.Walk(ctx, "", true, func(path, parent string, isDir bool) error {
				return nil
			}))
		})
		assert.Less(t, alloc, uint64(size/8), "Walk allocated %d bytes", alloc)
	})
}

func TestWalk(t *testing.T) {
	repo := newTestRepo(t, map[string][]byte{
		"model.config":       []byte("config"),
		"meshes/a.dae":       []byte("a"),
		"meshes/sub/b.dae":   []byte("b"),
		"materials/c.png":    []byte("c"),
		"thumbnails/1.png":   []byte("1"),
		"meshes/sub/c/d.stl": []byte("d"),
	})
	var files, folders []string
	err := repo.Walk(context.Background(), "", true, func(path, parent string, isDir bool) error {
		assert.Equal(t, filepath.Dir(path), parent)
		if isDir {
			folders = append(folders, path)
		} else {
			files = append(files, path)
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(files)
	sort.Strings(folders)
	assert.Equal(t, []string{"/materials/c.png", "/meshes/a.dae", "/meshes/sub/b.dae",
		"/meshes/sub/c/d.stl", "/model.config", "/thumbnails/1.png"}, files)
	assert.Equal(t, []string{"/materials", "/meshes", "/meshes/sub", "/meshes/sub/c",
		"/thumbnails"}, folders)
}
