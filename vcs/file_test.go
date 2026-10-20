package vcs

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useTempDir makes the temporary files be created in a new folder, which is
// returned.
func useTempDir(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

// assertEmptyDir asserts that dir has no files.
func assertEmptyDir(t *testing.T, dir string) {
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// useFakeGit makes the git command run the given shell script.
func useFakeGit(t *testing.T, script string) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script), 0755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// openTestFile returns the file with the given content, stored in a new
// repository.
func openTestFile(t *testing.T, data []byte) *File {
	repo := newTestRepo(t, map[string][]byte{"file": data})
	f, err := repo.OpenFile(context.Background(), "", "file")
	require.NoError(t, err)
	return f
}

func TestFileContentReadSeek(t *testing.T) {
	tmp := useTempDir(t)
	data := bytes.Repeat([]byte("0123456789"), 100)
	c, err := openTestFile(t, data).Content(context.Background())
	require.NoError(t, err)

	bs, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, data, bs)

	pos, err := c.Seek(-10, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(990), pos)
	buf := make([]byte, 5)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "01234", string(buf))

	_, err = c.Seek(3, io.SeekStart)
	require.NoError(t, err)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "34567", string(buf))

	require.NoError(t, c.Close())
	_, err = c.Read(buf)
	assert.ErrorIs(t, err, os.ErrClosed)
	assertEmptyDir(t, tmp)
}

func TestFileContentEmpty(t *testing.T) {
	c, err := openTestFile(t, nil).Content(context.Background())
	require.NoError(t, err)
	defer c.Close()
	bs, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Empty(t, bs)
}

func TestFileContentServeContent(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100)
	f := openTestFile(t, data)

	// serve serves the file. The file name has no extension, so that
	// ServeContent reads the content to detect its type.
	serve := func(rangeHeader string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/file", nil)
		if rangeHeader != "" {
			r.Header.Set("Range", rangeHeader)
		}
		w := httptest.NewRecorder()
		c, err := f.Content(context.Background())
		require.NoError(t, err)
		defer c.Close()
		http.ServeContent(w, r, "file", time.Now(), c)
		return w
	}

	w := serve("")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "1000", w.Header().Get("Content-Length"))
	assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, data, w.Body.Bytes())

	w = serve("bytes=995-")
	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "bytes 995-999/1000", w.Header().Get("Content-Range"))
	assert.Equal(t, "56789", w.Body.String())

	// The ranges of a multi-range request are sent in their order, even when
	// they go backwards.
	w = serve("bytes=10-14,2-3")
	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, []string{"01234", "23"}, readMultipart(t, w, "text/plain; charset=utf-8"))
}

// readMultipart returns the parts of a multipart/byteranges response, and
// checks their content type.
func readMultipart(t *testing.T, w *httptest.ResponseRecorder, contentType string) []string {
	mediaType, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/byteranges", mediaType)
	mr := multipart.NewReader(w.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return parts
		}
		require.NoError(t, err)
		assert.Equal(t, contentType, p.Header.Get("Content-Type"))
		bs, err := io.ReadAll(p)
		require.NoError(t, err)
		parts = append(parts, string(bs))
	}
}

func TestFileContentSizeMismatch(t *testing.T) {
	tmp := useTempDir(t)
	data := bytes.Repeat([]byte("0123456789"), 100)
	f := openTestFile(t, data)

	// git outputs less or more than the file size, and succeeds.
	for _, script := range []string{"head -c 999 " + "$CONTENT\n", "cat $CONTENT; echo extra\n"} {
		content := filepath.Join(t.TempDir(), "content")
		require.NoError(t, os.WriteFile(content, data, 0644))
		t.Setenv("CONTENT", content)
		useFakeGit(t, script)
		_, err := f.Content(context.Background())
		require.Error(t, err, script)
		assert.Contains(t, err.Error(), "expected 1000", script)
	}
	assertEmptyDir(t, tmp)
}

func TestFileContentGitFails(t *testing.T) {
	tmp := useTempDir(t)
	data := bytes.Repeat([]byte("0123456789"), 100)
	f := openTestFile(t, data)

	// git outputs the whole content, and then fails.
	content := filepath.Join(t.TempDir(), "content")
	require.NoError(t, os.WriteFile(content, data, 0644))
	useFakeGit(t, "cat "+content+"\necho failed >&2\nexit 1\n")

	_, err := f.Content(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 1")
	assert.Contains(t, err.Error(), "failed")
	assertEmptyDir(t, tmp)
}

func TestFileContentCanceled(t *testing.T) {
	tmp := useTempDir(t)
	f := openTestFile(t, []byte("content"))
	// A git that does not end.
	useFakeGit(t, "echo partial\nexec sleep 60\n")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := f.Content(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 30*time.Second)
	assertEmptyDir(t, tmp)

	// The context is done before the extraction starts.
	_, err = f.Content(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFileContentWaitsForExtractions(t *testing.T) {
	f := openTestFile(t, []byte("content"))
	// Take all the extraction slots.
	for i := 0; i < cap(extractions); i++ {
		extractions <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := f.Content(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Once a slot is free, the extraction runs.
	<-extractions
	c, err := f.Content(context.Background())
	require.NoError(t, err)
	require.NoError(t, c.Close())
	for i := 1; i < cap(extractions); i++ {
		<-extractions
	}
	assert.Empty(t, extractions)
}

func TestTempFileRemovedOnClose(t *testing.T) {
	tmp := useTempDir(t)
	tf, err := newTempFile()
	require.NoError(t, err)
	// The file is removed right away where possible, and otherwise on close.
	path := filepath.Join(tmp, filepath.Base(tf.File.Name()))
	_, err = tf.Write([]byte("data"))
	require.NoError(t, err)
	require.NoError(t, tf.Close())
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
	assertEmptyDir(t, tmp)
}
