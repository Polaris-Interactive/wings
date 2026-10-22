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
	"unicode/utf8"

	"emperror.dev/errors"
	"golang.org/x/sync/errgroup"

	"github.com/pterodactyl/wings/internal/ufs"
)

const (
	// searchMaxPatternLength is the longest search pattern that is accepted.
	searchMaxPatternLength = 1024
	// searchMaxLineLength is the longest line that is scanned. Scanning a file
	// stops at the first line longer than this, which is usually minified or
	// generated content.
	searchMaxLineLength = 1024 * 1024
	// searchBinaryPeek is how much of a file is checked for a NUL byte to decide
	// if it is a binary file, the same heuristic used by grep and git.
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

// SearchPatternError is returned when a search pattern cannot be used. Its
// message is safe to show to the user.
type SearchPatternError struct {
	msg string
}

func (e *SearchPatternError) Error() string {
	return e.msg
}

// SearchOptions controls a search of file contents.
type SearchOptions struct {
	// Root is the directory to search, every file beneath it is scanned.
	Root string
	// Pattern is the text to find, or a regular expression (RE2 syntax) when
	// Regex is set.
	Pattern       string
	Regex         bool
	CaseSensitive bool
	// MaxResults is the most files returned, MaxMatchesPerFile the most
	// matching lines returned for each file and MaxFileSize the largest file
	// scanned. Zero values use the defaults.
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

// SearchFile is a file that contains at least one match.
type SearchFile struct {
	Path    string        `json:"path"`
	Size    int64         `json:"size"`
	Matches []SearchMatch `json:"matches"`
	// Truncated is set when the file has more matches than were returned.
	Truncated bool `json:"truncated"`
}

// SearchResults is the result of a search.
type SearchResults struct {
	Files []SearchFile `json:"files"`
	// Truncated is set when the search stopped before every file was scanned,
	// because the result limit was reached or the context was cancelled.
	Truncated    bool `json:"truncated"`
	FilesScanned int  `json:"files_scanned"`
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

// compileSearchPattern returns the regular expression used to match lines.
// Go regular expressions run in linear time, so a user supplied pattern
// cannot be used to stall the search.
func compileSearchPattern(o SearchOptions) (*regexp.Regexp, error) {
	if o.Pattern == "" {
		return nil, &SearchPatternError{msg: "search pattern must not be empty"}
	}
	if len(o.Pattern) > searchMaxPatternLength {
		return nil, &SearchPatternError{msg: fmt.Sprintf("search pattern must not be longer than %d bytes", searchMaxPatternLength)}
	}
	p := o.Pattern
	if !o.Regex {
		p = regexp.QuoteMeta(p)
	}
	if !o.CaseSensitive {
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, &SearchPatternError{msg: "invalid regular expression: " + err.Error()}
	}
	return re, nil
}

// Search finds the files beneath a directory whose contents match a pattern.
// Symbolic links are not followed, and files on the denylist, binary files and
// files larger than the size limit are skipped. If the context is cancelled the
// results found so far are returned and marked as truncated.
func (fs *Filesystem) Search(ctx context.Context, o SearchOptions) (*SearchResults, error) {
	o.normalize()
	re, err := compileSearchPattern(o)
	if err != nil {
		return nil, err
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
		if d.IsDir() {
			if relative != "." && fs.IsIgnored(p+"/") != nil {
				return ufs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || fs.IsIgnored(p) != nil {
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
		if err != nil || !st.Mode().IsRegular() || st.Size() > o.MaxFileSize {
			_ = f.Close()
			return nil
		}

		g.Go(func() error {
			defer f.Close()
			matches, more := searchFile(ctx, f, re, o.MaxMatchesPerFile)

			mu.Lock()
			defer mu.Unlock()
			res.FilesScanned++
			if len(matches) == 0 {
				return nil
			}
			if len(res.Files) >= o.MaxResults {
				res.Truncated = true
				cancel()
				return nil
			}
			res.Files = append(res.Files, SearchFile{
				Path:      p,
				Size:      st.Size(),
				Matches:   matches,
				Truncated: more,
			})
			return nil
		})
		return nil
	})
	_ = g.Wait()

	if walkErr != nil {
		if !errors.Is(walkErr, context.Canceled) && !errors.Is(walkErr, context.DeadlineExceeded) {
			return nil, errors.WrapIf(walkErr, "server/filesystem: search: failed to walk directory")
		}
		res.Truncated = true
	}

	slices.SortFunc(res.Files, func(a, b SearchFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	return res, nil
}

// searchFile returns the lines of a file that match, up to limit of them, and
// whether there were more matches than that. Binary files return no matches.
func searchFile(ctx context.Context, f ufs.File, re *regexp.Regexp, limit int) ([]SearchMatch, bool) {
	r := bufio.NewReaderSize(f, 64*1024)
	head, _ := r.Peek(searchBinaryPeek)
	if bytes.IndexByte(head, 0) != -1 {
		return nil, false
	}

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
