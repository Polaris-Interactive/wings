package filesystem

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"emperror.dev/errors"
	"github.com/gabriel-vasile/mimetype"
	"golang.org/x/sync/errgroup"

	"github.com/pterodactyl/wings/internal/ufs"
)

const (
	// searchMaxPatternLength is the longest search query or name pattern that
	// is accepted.
	searchMaxPatternLength = 1024
	// searchMaxLineLength is the longest line that is scanned. Scanning a file
	// stops at the first line longer than this, which is usually minified or
	// generated content.
	searchMaxLineLength = 1024 * 1024
	// searchBinaryPeek is how much of a file is checked for a NUL byte to decide
	// if it is a binary file, the same heuristic used by grep and git. The same
	// bytes are used to detect the file's mimetype.
	searchBinaryPeek = 8 * 1024
	// searchPreviewLength is the most of a matching line returned in a result.
	searchPreviewLength = 256
	// searchWorkers is how many files are scanned at the same time.
	searchWorkers = 4

	SearchDefaultMaxResults           = 100
	SearchMaxResults                  = 1000
	SearchDefaultMatchesPerFile       = 10
	SearchMaxMatchesPerFile           = 100
	SearchDefaultMaxFileSize          = 10 * 1024 * 1024
	SearchMaxFileSize           int64 = 100 * 1024 * 1024
)

// Reasons a search stopped before every file was scanned.
const (
	SearchStoppedResults = "results"
	SearchStoppedTimeout = "timeout"
	SearchStoppedCancel  = "cancelled"
)

// SearchPatternError is returned when a search query or name pattern cannot be
// used. Its message is safe to show to the user.
type SearchPatternError struct {
	msg string
}

func (e *SearchPatternError) Error() string {
	return e.msg
}

// SearchOptions controls a search of files by name and by content. At least
// one of Query and Name must be set.
type SearchOptions struct {
	// Root is the directory to search, every file beneath it is considered.
	Root string
	// Query is the text to find inside files, or a regular expression (RE2
	// syntax) when Regex is set. When empty, files are matched by name alone.
	Query string
	Regex bool
	// Name filters on the file name. A pattern containing * or ? is matched as
	// a glob against the whole name, anything else as a substring of it.
	Name string
	// CaseSensitive applies to both Query and Name.
	CaseSensitive bool
	// IncludeHidden includes files and directories whose name starts with a dot.
	IncludeHidden bool
	// Exclude lists absolute paths that are skipped, along with everything
	// beneath them.
	Exclude []string
	// MaxResults is the most files returned, MaxMatchesPerFile the most
	// matching lines returned for each file and MaxFileSize the largest file
	// whose contents are scanned. Zero values use the defaults.
	MaxResults        int
	MaxMatchesPerFile int
	MaxFileSize       int64
}

// SearchMatch is a line of a file that matched the search.
type SearchMatch struct {
	// Line is the 1-based line number of the match.
	Line int `json:"line"`
	// Column is the 1-based byte offset of the match within the line.
	Column int `json:"column"`
	// Text is the matching line, shortened around the match when it is long.
	Text string `json:"text"`
}

// SearchFile is a file that matched the search.
type SearchFile struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Directory  string    `json:"directory"`
	Size       int64     `json:"size"`
	Mimetype   string    `json:"mimetype"`
	ModifiedAt time.Time `json:"modified_at"`
	// Matches is empty when the search matched on the file name alone.
	Matches []SearchMatch `json:"matches"`
	// Truncated is set when the file has more matches than were returned.
	Truncated bool `json:"truncated"`
}

// SearchResults is the result of a search.
type SearchResults struct {
	Files []SearchFile `json:"files"`
	// Truncated is set when the search stopped before every file was scanned,
	// and Reason is why: the result limit was reached, it ran out of time, or
	// it was cancelled.
	Truncated          bool   `json:"truncated"`
	Reason             string `json:"reason,omitempty"`
	FilesScanned       int    `json:"files_scanned"`
	DirectoriesScanned int    `json:"directories_scanned"`
	DurationMs         int64  `json:"duration_ms"`
}

func (o *SearchOptions) normalize() {
	if o.MaxResults <= 0 {
		o.MaxResults = SearchDefaultMaxResults
	}
	o.MaxResults = min(o.MaxResults, SearchMaxResults)
	if o.MaxMatchesPerFile <= 0 {
		o.MaxMatchesPerFile = SearchDefaultMatchesPerFile
	}
	o.MaxMatchesPerFile = min(o.MaxMatchesPerFile, SearchMaxMatchesPerFile)
	if o.MaxFileSize <= 0 {
		o.MaxFileSize = SearchDefaultMaxFileSize
	}
	o.MaxFileSize = min(o.MaxFileSize, SearchMaxFileSize)
}

// searchMatchers are the compiled forms of a search's query and name filter.
// Either is nil when the search does not use it. Go regular expressions run in
// linear time, so a user supplied pattern cannot be used to stall the search.
type searchMatchers struct {
	query *regexp.Regexp
	name  *regexp.Regexp
}

func compileSearch(o SearchOptions) (searchMatchers, error) {
	var m searchMatchers
	if o.Query == "" && o.Name == "" {
		return m, &SearchPatternError{msg: "a search needs a query or a file name pattern"}
	}
	if len(o.Query) > searchMaxPatternLength || len(o.Name) > searchMaxPatternLength {
		return m, &SearchPatternError{msg: fmt.Sprintf("search patterns must not be longer than %d bytes", searchMaxPatternLength)}
	}
	flags := ""
	if !o.CaseSensitive {
		flags = "(?i)"
	}
	if o.Query != "" {
		p := o.Query
		if !o.Regex {
			p = regexp.QuoteMeta(p)
		}
		re, err := regexp.Compile(flags + p)
		if err != nil {
			return m, &SearchPatternError{msg: "invalid regular expression: " + err.Error()}
		}
		m.query = re
	}
	if o.Name != "" {
		p := regexp.QuoteMeta(o.Name)
		if strings.ContainsAny(o.Name, "*?") {
			p = "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(p) + "$"
		}
		m.name = regexp.MustCompile(flags + p)
	}
	return m, nil
}

// excluded reports whether p is one of the excluded paths or beneath one.
func excluded(p string, exclude []string) bool {
	for _, e := range exclude {
		if p == e || strings.HasPrefix(p, strings.TrimSuffix(e, "/")+"/") {
			return true
		}
	}
	return false
}

// Search finds the files beneath a directory whose name and contents match.
// Symbolic links are not followed, and files on the denylist are skipped. A
// content search also skips binary files and files larger than the size limit.
// If the context is cancelled or its deadline passes, the results found so far
// are returned and marked as truncated.
func (fs *Filesystem) Search(ctx context.Context, o SearchOptions) (*SearchResults, error) {
	started := time.Now()
	o.normalize()
	m, err := compileSearch(o)
	if err != nil {
		return nil, err
	}

	exclude := make([]string, 0, len(o.Exclude))
	for _, e := range o.Exclude {
		if e = strings.TrimSpace(e); e != "" {
			exclude = append(exclude, path.Clean("/"+strings.TrimLeft(e, "/")))
		}
	}

	root := path.Clean("/" + strings.TrimLeft(o.Root, "/"))
	// The root is a directory, so it must also be checked against patterns that
	// only match directories.
	if root != "/" {
		if err := fs.IsIgnored(root, root+"/"); err != nil {
			return nil, err
		}
	}

	dirfd, name, closeFd, err := fs.unixFS.SafePath(root)
	defer closeFd()
	if err != nil {
		return nil, err
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu  sync.Mutex
		res = &SearchResults{Files: []SearchFile{}}
	)

	var g errgroup.Group
	g.SetLimit(searchWorkers)

	walkErr := fs.unixFS.WalkDirat(dirfd, name, func(dirfd int, name, relative string, d ufs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// The root itself must be readable, anything below it that cannot be
			// read or was removed during the walk is skipped.
			if relative == "." {
				return err
			}
			if d != nil && d.IsDir() {
				return ufs.SkipDir
			}
			return nil
		}

		p := path.Join(root, relative)
		if relative != "." {
			if (!o.IncludeHidden && strings.HasPrefix(d.Name(), ".")) || excluded(p, exclude) {
				if d.IsDir() {
					return ufs.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			if relative != "." && fs.IsIgnored(p+"/") != nil {
				return ufs.SkipDir
			}
			mu.Lock()
			res.DirectoriesScanned++
			mu.Unlock()
			return nil
		}
		if !d.Type().IsRegular() || fs.IsIgnored(p) != nil {
			return nil
		}
		if m.name != nil && !m.name.MatchString(d.Name()) {
			return nil
		}

		// Files are opened here, as dirfd is closed once the walk leaves the
		// directory. Opening never follows a symbolic link and refuses anything
		// that is not a regular file, should the file be replaced after it was
		// read from the directory.
		f, err := fs.unixFS.OpenFileat(dirfd, name, ufs.O_RDONLY, 0)
		if err != nil {
			return nil
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || (m.query != nil && (st.Size() == 0 || st.Size() > o.MaxFileSize)) {
			_ = f.Close()
			return nil
		}

		g.Go(func() error {
			defer f.Close()
			r := bufio.NewReaderSize(f, 64*1024)
			head, _ := r.Peek(searchBinaryPeek)

			var (
				matches []SearchMatch
				more    bool
			)
			if m.query != nil {
				if bytes.IndexByte(head, 0) != -1 {
					// Binary files are never searched for content.
					mu.Lock()
					res.FilesScanned++
					mu.Unlock()
					return nil
				}
				matches, more = searchLines(ctx, r, m.query, o.MaxMatchesPerFile)
			}

			mu.Lock()
			defer mu.Unlock()
			res.FilesScanned++
			if m.query != nil && len(matches) == 0 {
				return nil
			}
			if len(res.Files) >= o.MaxResults {
				res.Reason = SearchStoppedResults
				cancel()
				return nil
			}
			if matches == nil {
				matches = []SearchMatch{}
			}
			res.Files = append(res.Files, SearchFile{
				Path:       p,
				Name:       d.Name(),
				Directory:  path.Dir(p),
				Size:       st.Size(),
				Mimetype:   mimetype.Detect(head).String(),
				ModifiedAt: st.ModTime(),
				Matches:    matches,
				Truncated:  more,
			})
			return nil
		})
		return nil
	})
	_ = g.Wait()

	if walkErr != nil && !errors.Is(walkErr, context.Canceled) && !errors.Is(walkErr, context.DeadlineExceeded) {
		return nil, errors.WrapIf(walkErr, "server/filesystem: search: failed to walk directory")
	}
	// A file being scanned when the search was stopped may have been cut short,
	// so the search is truncated whatever stopped it.
	if res.Reason == "" && parent.Err() != nil {
		res.Reason = SearchStoppedCancel
		if errors.Is(parent.Err(), context.DeadlineExceeded) {
			res.Reason = SearchStoppedTimeout
		}
	}
	res.Truncated = res.Reason != ""

	slices.SortFunc(res.Files, func(a, b SearchFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	res.DurationMs = time.Since(started).Milliseconds()
	return res, nil
}

// searchLines returns the lines read from r that match, up to limit of them,
// and whether there were more matches than that.
func searchLines(ctx context.Context, r *bufio.Reader, re *regexp.Regexp, limit int) ([]SearchMatch, bool) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), searchMaxLineLength)

	var matches []SearchMatch
	for line := 1; s.Scan(); line++ {
		// Checking every line would be wasteful, but a large file should still
		// stop promptly once the search is cancelled.
		if line%1024 == 0 && ctx.Err() != nil {
			break
		}
		b := s.Bytes()
		loc := re.FindIndex(b)
		if loc == nil {
			continue
		}
		if len(matches) == limit {
			return matches, true
		}
		matches = append(matches, SearchMatch{
			Line:   line,
			Column: loc[0] + 1,
			Text:   searchPreview(b, loc[0], loc[1]),
		})
	}
	return matches, false
}

// searchPreview returns the line containing a match, shortened to a window
// around the match when the line is long.
func searchPreview(line []byte, start, end int) string {
	line = bytes.TrimRight(line, "\r")
	if len(line) > searchPreviewLength {
		// Keep some context before the match, and as much of the match and
		// what follows as fits.
		from := max(0, start-searchPreviewLength/4)
		to := min(len(line), max(end, from+searchPreviewLength))
		to = min(to, from+searchPreviewLength)
		// Don't cut a multi-byte character in half.
		for from > 0 && !utf8.RuneStart(line[from]) {
			from--
		}
		for to < len(line) && !utf8.RuneStart(line[to]) {
			to++
		}
		line = line[from:to]
	}
	return strings.ToValidUTF8(string(line), "�")
}
