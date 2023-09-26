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

	"regexp"
	"strings"
	"sync"

	// regexp "github.com/wasilibs/go-re2" // TODO decide between both libraries

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

var tags = []string{"BUG", "FIXME", "XXX", "TODO", "HACK", "OPTIMIZE", "NOTE"}

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
	Author string
	Date   string
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
				Author: strings.TrimPrefix(buf, "author "),
			}
		} else if strings.HasPrefix(buf, "author-time ") {
			if currentBlame != nil {
				currentBlame.Date = strings.TrimPrefix(buf, "author-time ")
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
	path  string
	lines []*matchLine
}

func Search(path string, regex *regexp.Regexp, matcher gitignore.Matcher, debug *Options) {
	searchJobs := make(chan *searchJob)
	searchResults := make(chan *searchResult)

	var wg sync.WaitGroup
	var wgResult sync.WaitGroup
	for w := 0; w < debug.Workers; w++ {
		go searchWorker(searchJobs, searchResults, matcher, &wg, &wgResult)
	}

	for w := 0; w < debug.Workers; w++ {
		go processResult(searchResults, &wgResult)
	}

	filepath.WalkDir(
		path,
		func(path string, d fs.DirEntry, err error) error { return walk(path, d, err, regex, searchJobs, &wg) },
	)
	wg.Wait()
	wgResult.Wait()
}

func processResult(searchResults chan *searchResult, wgResult *sync.WaitGroup) {
	for result := range searchResults {
		gb, gb_err := BlameFile(result.path)
		for _, line := range result.lines {
			var blame *LineBlame
			var err error
			if gb_err == nil {
				blame, err = gb.BlameLine(line.n)
			} else {
				// fmt.Printf("foo bar %v\n", gb_err)
			}
			if gb_err == nil && err == nil {
				fmt.Printf("%s [Line %d] %s: %s [%s]\n", result.path, line.n, line.tag, line.text, blame.Author)
			} else {
				// fmt.Printf("blame err: %v\n", gb_err)
				fmt.Printf("%s [Line %d] %s: %s\n", result.path, line.n, line.tag, line.text)
			}
		}
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
			fmt.Printf("skipping %s due to .gitignore\n", job.path)
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
			searchResults <- &searchResult{path: job.path, lines: lines}
		}
		wg.Done()
	}
}
