package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	ignore "github.com/sabhiram/go-gitignore"
	"github.com/stretchr/testify/require"
)

func newSearchFs(t *testing.T, files map[string]string) (*Filesystem, *rootFs) {
	t.Helper()
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	for p, c := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(rfs.root, "server", filepath.Dir(p)), 0o755))
		require.NoError(t, rfs.CreateServerFileFromString(p, c))
	}
	return fs, rfs
}

func searchPaths(res *SearchResults) []string {
	out := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestSearch(t *testing.T) {
	fs, rfs := newSearchFs(t, map[string]string{
		"server.properties":             "motd=A Minecraft Server\nlevel-name=world\n",
		"plugins/Essentials/config.yml": "# config\ndatabase:\n  password: Hunter2\n",
		"plugins/Other/config.yml":      "nothing here\n",
		"logs/latest.log":               "[INFO] hunter2 joined\n[INFO] HUNTER2 left\n",
	})

	t.Run("literal search is case-insensitive by default", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Query: "hunter2"})
		require.NoError(t, err)
		require.False(t, res.Truncated)
		require.Equal(t, 4, res.FilesScanned)
		require.Equal(t, []string{"/logs/latest.log", "/plugins/Essentials/config.yml"}, searchPaths(res))

		f := res.Files[1]
		require.Len(t, f.Matches, 1)
		require.Equal(t, SearchMatch{Line: 3, Column: 13, Text: "  password: Hunter2"}, f.Matches[0])
		require.Len(t, res.Files[0].Matches, 2)
	})

	t.Run("case-sensitive search", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Query: "HUNTER2", CaseSensitive: true})
		require.NoError(t, err)
		require.Equal(t, []string{"/logs/latest.log"}, searchPaths(res))
		require.Equal(t, 2, res.Files[0].Matches[0].Line)
	})

	t.Run("literal patterns are not regular expressions", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Query: "level.name"})
		require.NoError(t, err)
		require.Empty(t, res.Files)
	})

	t.Run("regular expression search", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Query: `^level.name=\w+$`, Regex: true})
		require.NoError(t, err)
		require.Equal(t, []string{"/server.properties"}, searchPaths(res))
	})

	t.Run("invalid patterns", func(t *testing.T) {
		for _, o := range []SearchOptions{
			{Query: ""},
			{Name: strings.Repeat("a", searchMaxPatternLength+1)},
			{Query: "(unclosed", Regex: true},
			{Query: strings.Repeat("a", searchMaxPatternLength+1)},
		} {
			_, err := fs.Search(context.Background(), o)
			var perr *SearchPatternError
			require.True(t, errors.As(err, &perr), "expected a pattern error for %q, got %v", o.Query, err)
		}
	})

	t.Run("search is limited to the root directory", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Root: "/plugins", Query: "config"})
		require.NoError(t, err)
		require.Equal(t, 2, res.FilesScanned)
		require.Equal(t, []string{"/plugins/Essentials/config.yml"}, searchPaths(res))
	})

	t.Run("root outside the server cannot be searched", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(rfs.root, "outside.txt"), []byte("hunter2"), 0o644))
		res, err := fs.Search(context.Background(), SearchOptions{Root: "../", Query: "hunter2"})
		if err == nil {
			for _, f := range res.Files {
				require.NotContains(t, f.Path, "outside")
			}
		}
	})
}

func TestSearchSkipsFiles(t *testing.T) {
	fs, rfs := newSearchFs(t, map[string]string{
		"visible.txt":        "secret",
		"denied.txt":         "secret",
		"denied-dir/a.txt":   "secret",
		"nested/denied2.txt": "secret",
		"big.txt":            strings.Repeat("x", 2048) + "secret",
	})
	fs.denylist = ignore.CompileIgnoreLines("denied.txt", "denied-dir/", "**/denied2.txt")

	// A binary file that contains the pattern.
	require.NoError(t, rfs.CreateServerFile("world.dat", append([]byte("secret"), 0, 1, 2)))
	// A symbolic link to a file outside the server that contains the pattern.
	require.NoError(t, os.WriteFile(filepath.Join(rfs.root, "outside.txt"), []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(rfs.root, "outside.txt"), filepath.Join(rfs.root, "server/link.txt")))
	require.NoError(t, os.Symlink(rfs.root, filepath.Join(rfs.root, "server/linkdir")))

	res, err := fs.Search(context.Background(), SearchOptions{Query: "secret", MaxFileSize: 1024})
	require.NoError(t, err)
	require.Equal(t, []string{"/visible.txt"}, searchPaths(res))

	res, err = fs.Search(context.Background(), SearchOptions{Query: "secret"})
	require.NoError(t, err)
	require.Equal(t, []string{"/big.txt", "/visible.txt"}, searchPaths(res))

	_, err = fs.Search(context.Background(), SearchOptions{Root: "/denied-dir", Query: "secret"})
	require.True(t, IsErrorCode(err, ErrCodeDenylistFile))
}

func TestSearchLimits(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files[n+".txt"] = strings.Repeat("match\n", 20)
	}
	fs, _ := newSearchFs(t, files)

	res, err := fs.Search(context.Background(), SearchOptions{Query: "match", MaxResults: 2, MaxMatchesPerFile: 3})
	require.NoError(t, err)
	require.True(t, res.Truncated)
	require.Equal(t, SearchStoppedResults, res.Reason)
	require.Len(t, res.Files, 2)
	for _, f := range res.Files {
		require.True(t, f.Truncated)
		require.Len(t, f.Matches, 3)
		require.Equal(t, []int{1, 2, 3}, []int{f.Matches[0].Line, f.Matches[1].Line, f.Matches[2].Line})
	}

	res, err = fs.Search(context.Background(), SearchOptions{Query: "match", MaxResults: 5, MaxMatchesPerFile: 20})
	require.NoError(t, err)
	require.False(t, res.Truncated)
	require.Len(t, res.Files, 5)
	require.False(t, res.Files[0].Truncated)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = fs.Search(ctx, SearchOptions{Query: "match"})
	require.NoError(t, err)
	require.True(t, res.Truncated)
	require.Equal(t, SearchStoppedCancel, res.Reason)
}

func TestSearchPreview(t *testing.T) {
	long := strings.Repeat("é", 300) + "needle" + strings.Repeat("ü", 300)
	start := strings.Index(long, "needle")
	p := searchPreview([]byte(long), start, start+len("needle"))
	require.True(t, utf8.ValidString(p))
	require.Contains(t, p, "needle")
	require.LessOrEqual(t, len(p), searchPreviewLength+utf8.UTFMax)

	require.Equal(t, "short line", searchPreview([]byte("short line\r"), 0, 5))
}

func TestSearchByName(t *testing.T) {
	fs, _ := newSearchFs(t, map[string]string{
		"server.properties":        "motd=hello\n",
		"plugins/A/config.yml":     "motd: hello\n",
		"plugins/B/Config.YML":     "other\n",
		"plugins/B/messages.yml":   "motd: hello\n",
		".hidden/config.yml":       "motd: hello\n",
		"plugins/.secret.yml":      "motd: hello\n",
		".pterodactyl-trash/a.yml": "motd: hello\n",
		"plugins/A/config.yml.bak": "motd: hello\n",
	})

	t.Run("substring name filter without a query", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Name: "config"})
		require.NoError(t, err)
		require.Equal(t, []string{"/plugins/A/config.yml", "/plugins/A/config.yml.bak", "/plugins/B/Config.YML"}, searchPaths(res))
		f := res.Files[0]
		require.Equal(t, "config.yml", f.Name)
		require.Equal(t, "/plugins/A", f.Directory)
		require.Equal(t, int64(12), f.Size)
		require.Contains(t, f.Mimetype, "text/plain")
		require.False(t, f.ModifiedAt.IsZero())
		require.NotNil(t, f.Matches)
		require.Empty(t, f.Matches)
	})

	t.Run("glob name filter matches the whole name", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Name: "*.yml", CaseSensitive: true})
		require.NoError(t, err)
		require.Equal(t, []string{"/plugins/A/config.yml", "/plugins/B/messages.yml"}, searchPaths(res))
	})

	t.Run("name filter narrows a content search", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Name: "*.yml", Query: "motd"})
		require.NoError(t, err)
		require.Equal(t, []string{"/plugins/A/config.yml", "/plugins/B/messages.yml"}, searchPaths(res))
		require.Equal(t, 1, res.Files[0].Matches[0].Line)
	})

	t.Run("hidden files and excluded paths", func(t *testing.T) {
		res, err := fs.Search(context.Background(), SearchOptions{Query: "motd", IncludeHidden: true, Exclude: []string{"/.pterodactyl-trash"}})
		require.NoError(t, err)
		require.Equal(t, []string{"/.hidden/config.yml", "/plugins/.secret.yml", "/plugins/A/config.yml", "/plugins/A/config.yml.bak", "/plugins/B/messages.yml", "/server.properties"}, searchPaths(res))

		res, err = fs.Search(context.Background(), SearchOptions{Query: "motd", Exclude: []string{"plugins/A/"}})
		require.NoError(t, err)
		require.Equal(t, []string{"/plugins/B/messages.yml", "/server.properties"}, searchPaths(res))
	})

	t.Run("deadline marks the search as timed out", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		res, err := fs.Search(ctx, SearchOptions{Query: "motd"})
		require.NoError(t, err)
		require.True(t, res.Truncated)
		require.Equal(t, SearchStoppedTimeout, res.Reason)
	})
}
