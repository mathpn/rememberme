package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"regexp"
	"strings"
	"sync"

	// regexp "github.com/wasilibs/go-re2" // TODO decide between both libraries

	"github.com/charmbracelet/lipgloss"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"golang.org/x/sys/unix"
)

var tags = []string{"BUG", "FIXME", "XXX", "TODO", "HACK", "OPTIMIZE", "NOTE"}

const maxAuthorLength = 22

func BlameFile(path string) (*GitBlame, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	err = os.Chdir(filepath.Dir(absolutePath))
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("git", "blame", absolutePath, "--line-porcelain")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	blameChannel := make(chan []*LineBlame, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		parseGitBlame(stdout, blameChannel)
	}()

	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git blame failed: %v\n%s", err, stderr.String())
	}

	blames := <-blameChannel
	return &GitBlame{blames: blames}, nil
}

type LineBlame struct {
	Author    string
	Timestamp int64
}

type GitBlame struct {
	blames []*LineBlame
}

func (b *GitBlame) BlameLine(line int) (*LineBlame, error) {
	line = line - 1
	if line < 0 || line >= len(b.blames) {
		return nil, fmt.Errorf("line out of range")
	}
	return b.blames[line], nil
}

func truncateName(name string, maxLength int) string {
	totalLen := len(name)
	words := strings.Fields(name) // Split the name into words

	truncated := []string{}
	for i := len(words) - 1; i >= 0; i-- {
		if totalLen > maxLength {
			truncated = append(truncated, string(words[i][0])) // First letter
			totalLen -= len(words[i]) - 2
		} else {
			truncated = append(truncated, words[i])
		}
	}

	for i, j := 0, len(truncated)-1; i < j; i, j = i+1, j-1 {
		truncated[i], truncated[j] = truncated[j], truncated[i]
	}

	return strings.Join(truncated, " ")
}

// TODO truncate git author length
func parseGitBlame(out io.Reader, blameChannel chan []*LineBlame) {
	var blames []*LineBlame
	lr := bufio.NewReader(out) // XXX NewReaderSize?
	s := bufio.NewScanner(lr)

	var currentBlame *LineBlame
	for s.Scan() {
		buf := s.Text()
		if strings.HasPrefix(buf, "author ") {
			if currentBlame != nil {
				blames = append(blames, currentBlame)
			}
			currentBlame = &LineBlame{
				Author: truncateName(strings.TrimPrefix(buf, "author "), maxAuthorLength),
			}
		} else if strings.HasPrefix(buf, "author-time ") {
			if currentBlame != nil {
				tsStr := strings.TrimPrefix(buf, "author-time ")
				ts, err := strconv.ParseInt(tsStr, 10, 64)
				if err == nil {
					currentBlame.Timestamp = ts
				}
			}
		}
	}

	// Append the last entry
	if currentBlame != nil {
		blames = append(blames, currentBlame)
	}

	blameChannel <- blames
}

func parseArgs(flagArgs []string) (string, string) {
	if len(flagArgs) < 1 {
		log.Fatal("usage: listme <path>")
	}
	tags_regex := fmt.Sprintf(
		`(?m)(?:^|\s*(?:(?:#+|//+|<!--|--|/*|"""|''')+\s*)+)\s*(?:^|\b)(%s)[\s:;-]+(.+?)(?:$|-->|#}}|\*/|--}}|}}|#+|#}|"""|''')*$`,
		strings.Join(tags, "|"),
	)
	// fmt.Println(tags_regex)
	return tags_regex, flagArgs[0]
}

func loadGitignore(path string) (gitignore.Matcher, error) {
	repo, err := git.PlainOpenWithOptions(path, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	rootDir := wt.Filesystem

	pattern, err := gitignore.ReadPatterns(rootDir, []string{})
	if err != nil {
		return nil, err
	}
	matcher := gitignore.NewMatcher(pattern)
	return matcher, nil
}

func main() {
	var workers = flag.Int("w", 128, "[debug] set number of search workers")
	flag.Parse()

	query, path := parseArgs(flag.Args())
	opt := &Options{
		Workers: *workers,
	}

	r, err := regexp.Compile(query)
	if err != nil {
		log.Fatalf("Bad regex: %s", err)
	}

	matcher, _ := loadGitignore(path)
	Search(path, r, matcher, opt)
}

type Options struct {
	Workers int
}

type searchJob struct {
	path  string
	regex *regexp.Regexp
}

type matchLine struct {
	n    int
	tag  string
	text string
}

type searchResult struct {
	Path  string
	Lines []*matchLine
}

func (r *searchResult) MaxLineNumber() int {
	max := 0
	for _, line := range r.Lines {
		if line.n > max {
			max = line.n
		}
	}
	return max
}

type Style int

const (
	FullStyle = iota
	BWStyle
	PlainStyle
)

func Search(path string, regex *regexp.Regexp, matcher gitignore.Matcher, debug *Options) {
	searchJobs := make(chan *searchJob)
	searchResults := make(chan *searchResult)

	var wg sync.WaitGroup
	var wgResult sync.WaitGroup
	for w := 0; w < debug.Workers; w++ {
		go searchWorker(searchJobs, searchResults, matcher, &wg, &wgResult)
	}

	go PrintResult(searchResults, &wgResult)

	filepath.WalkDir(
		path,
		func(path string, d fs.DirEntry, err error) error { return walk(path, d, err, regex, searchJobs, &wg) },
	)
	wg.Wait()
	wgResult.Wait()
}

// TODO organize code
var BaseStyle = lipgloss.NewStyle()
var BoldStyle = BaseStyle.Copy().Bold(true)
var FilenameColorStyle = BoldStyle.Copy().Foreground(lipgloss.Color("#0087d7"))

func stylizeFilename(file string, nComments int, style Style) string {
	styler := BaseStyle
	if style == BWStyle {
		styler = BoldStyle
	} else if style == FullStyle {
		styler = FilenameColorStyle
	}
	fname := styler.Render(fmt.Sprintf("• %s", file))
	var comments string
	if nComments > 1 {
		comments = styler.Render(fmt.Sprintf("(%d comments)", nComments))
	} else {
		comments = styler.Render(fmt.Sprintf("(%d comment)", nComments))
	}
	return fname + " " + comments
}

func emojify(tag string) string {
	// TODO extra symbols support config
	switch tag {
	case "TODO":
		return "✓ TODO"
	case "XXX":
		return "✘ XXX"
	case "FIXME":
		return "⚠ FIXME"
	case "OPTIMIZE":
		return " OPTIMIZE"
	case "BUG":
		return "☢ BUG"
	case "NOTE":
		return "✐ NOTE"
	case "HACK":
		return "✄ HACK"
	}
	return "⚠ " + tag
}

var TodoStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#5fafaf"))
var XxxStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#d7af00"))
var FixmeStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#ff0000"))
var OptimizeStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#d75f00"))
var BugStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#eeeeee")).Background(lipgloss.Color("#870000"))
var NoteStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#87af87"))
var HackStyle = BaseStyle.Copy().Foreground(lipgloss.Color("#d7d700"))

func colorize(text string, tag string) string {
	switch tag {
	case "TODO":
		return TodoStyle.Render(text)
	case "XXX":
		return XxxStyle.Render(text)
	case "FIXME":
		return FixmeStyle.Render(text)
	case "OPTIMIZE":
		return OptimizeStyle.Render(text)
	case "BUG":
		return BugStyle.Render(text)
	case "NOTE":
		return NoteStyle.Render(text)
	case "HACK":
		return HackStyle.Render(text)
	}
	return text
}

// TODO slighly inefficient (wasted computations)
func padLineNumber(number int, maxNumber int) string {
	strNumber := fmt.Sprint(number)
	strMaxNumber := fmt.Sprint(maxNumber)
	pad := strings.Repeat(" ", len(strMaxNumber)-len(strNumber))
	return fmt.Sprintf("[Line %s%d] ", pad, number)
}

func prettiyfyLine(text string, tag string, style Style) string {
	prettyTag := BoldStyle.Render(emojify(tag))
	text = " " + text
	if style == FullStyle {
		prettyTag = colorize(prettyTag, tag)
		text = colorize(text, tag)
	}
	return prettyTag + text
}

var OldCommitStyle = BoldStyle.Copy().Foreground(lipgloss.Color("#dadada")).Background(lipgloss.Color("#d70000"))

// TODO right-align blame
func prettiyfyBlame(blame *LineBlame, ageLimit int, style Style) string {
	if style == PlainStyle {
		return ""
	}

	blameStr := fmt.Sprintf("[%s]", blame.Author)
	if blame.Timestamp == 0 {
		return blameStr
	}
	date := time.Unix(blame.Timestamp, 0)
	currentDate := time.Now()

	diff := currentDate.Sub(date)
	maxAge := 30 * 24 * time.Hour
	if diff > maxAge {
		blameStr := fmt.Sprintf("[☠ OLD %s]", blame.Author)
		return OldCommitStyle.Render(blameStr)
	}
	return blameStr
}

const STYLE = FullStyle // TODO parameter
const AGELIMIT = 30     // TODO parameter

func PrintResult(searchResults chan *searchResult, wgResult *sync.WaitGroup) {
	for result := range searchResults {
		fmt.Println(stylizeFilename(result.Path, len(result.Lines), STYLE))
		gb, gb_err := BlameFile(result.Path)
		for _, line := range result.Lines {
			text := prettiyfyLine(line.text, line.tag, STYLE)
			lineNumber := padLineNumber(line.n, result.MaxLineNumber())
			var blame *LineBlame
			var err error
			if gb_err == nil {
				blame, err = gb.BlameLine(line.n)
			}
			if gb_err == nil && err == nil {
				blameStr := prettiyfyBlame(blame, AGELIMIT, STYLE)
				fmt.Println(lineNumber + text + " " + blameStr)
			} else {
				fmt.Println(lineNumber + text)
			}
		}
		fmt.Println()
		wgResult.Done()
	}
}

func walk(path string, d fs.DirEntry, err error, regex *regexp.Regexp, searchJobs chan *searchJob, wg *sync.WaitGroup) error {
	if err != nil {
		return err
	}
	if !d.IsDir() {
		wg.Add(1)
		searchJobs <- &searchJob{
			path,
			regex,
		}
	}
	return nil
}

func searchWorker(jobs chan *searchJob, searchResults chan *searchResult, matcher gitignore.Matcher, wg *sync.WaitGroup, wgResult *sync.WaitGroup) {
	for job := range jobs {
		info, err := os.Stat(job.path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("%s does not exist.\n", job.path)
			} else {
				fmt.Printf("Error checking %s: %v\n", job.path, err)
			}
			return
		}

		pathList := strings.Split(job.path, string(filepath.Separator))
		if matcher != nil && matcher.Match(pathList, info.IsDir()) {
			// fmt.Printf("skipping %s due to .gitignore\n", job.path)
			wg.Done()
			continue
		}
		f, err := os.Open(job.path)
		if err != nil {
			log.Fatalf("couldn't open path %s: %s\n", job.path, err)
		}

		scanner := bufio.NewScanner(f)

		line := 1
		lines := make([]*matchLine, 0)
		for scanner.Scan() {
			text := scanner.Bytes()

			if mimeType := http.DetectContentType(text); strings.Split(mimeType, ";")[0] != "text/plain" {
				// fmt.Printf("skipping non-text file: %s | %s\n", job.path, mimeType)
				break
			}

			match := job.regex.FindSubmatch(scanner.Bytes())
			if len(match) >= 3 {
				line := matchLine{n: line, tag: string(match[1]), text: string(match[2])}
				lines = append(lines, &line)
				// fmt.Printf("%v\n", line)
			}
			line++
		}
		if len(lines) > 0 {
			wgResult.Add(1)
			searchResults <- &searchResult{Path: job.path, Lines: lines}
		}
		wg.Done()
	}
}
