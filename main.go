package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go/ast"
	"go/parser"
	"go/token"

	"github.com/google/go-github/v48/github"
	"golang.org/x/oauth2"
)

// Global counters for ratio calculation
var (
	totalStrings   int
	matchedStrings int
)

// matchInfo holds details about each matched line/string.
type matchInfo struct {
	File       string
	LineNumber int
	Identifier string
	Literal    string
}

func main() {
	var (
		minStars   int
		maxStars   int
		maxResults int
	)
	flag.IntVar(&minStars, "stars", 1000, "Minimum number of stars")
	flag.IntVar(&maxStars, "maxstars", 7500, "Maximum number of stars")
	flag.IntVar(&maxResults, "max", 5, "Max number of repositories to process")
	flag.Parse()

	ctx := context.Background()
	tc := oauth2.NewClient(ctx, nil)

	client := github.NewClient(tc)

	// Search for Go repos with >= minStars and < maxStars stars
	query := fmt.Sprintf("language:Go stars:%d..%d", minStars, maxStars)
	searchOpts := &github.SearchOptions{
		Sort:  "stars",
		Order: "desc",
		ListOptions: github.ListOptions{
			PerPage: maxResults,
		},
	}

	result, _, err := client.Search.Repositories(ctx, query, searchOpts)
	if err != nil {
		log.Fatalf("Error searching repositories: %v", err)
	}

	for i, repo := range result.Repositories {
		if i >= maxResults {
			break
		}
		log.Printf("Scanning repository %s (stars=%d)\n",
			repo.GetFullName(), repo.GetStargazersCount())

		tmpDir := fmt.Sprintf("repo-%s", strings.ReplaceAll(repo.GetFullName(), "/", "-"))
		if err := cloneRepo(repo.GetCloneURL(), tmpDir); err != nil {
			log.Printf("Error cloning %s: %v", repo.GetFullName(), err)
			continue
		}
		defer os.RemoveAll(tmpDir)

		// Walk all .go files
		goFiles := []string{}
		_ = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(path, ".go") {
				goFiles = append(goFiles, path)
			}
			return nil
		})

		var totalLines, stringLines, repoTotalStrings, repoMatchedStrings int
		allMatches := make([]matchInfo, 0) // collect matches for each repo

		for _, fpath := range goFiles {
			lines, strLines, matchedStrs, matches := analyzeFileWithLines(fpath)
			totalLines += lines
			stringLines += strLines
			repoTotalStrings += strLines
			repoMatchedStrings += matchedStrs

			if len(matches) > 0 {
				allMatches = append(allMatches, matches...)
			}
		}

		// Calculate ratios for the repository
		repoLineRatio := 0.0
		repoStringRatio := 0.0
		if totalLines > 0 {
			repoLineRatio = float64(stringLines) / float64(totalLines)
		}
		if repoTotalStrings > 0 {
			repoStringRatio = float64(repoMatchedStrings) / float64(repoTotalStrings)
		}
		log.Printf("Repository %s: String to total line ratio: %.4f (%d string lines / %d total lines)\n",
			repo.GetFullName(), repoLineRatio, stringLines, totalLines)
		log.Printf("Repository %s: Matched strings to total strings ratio: %.4f (%d matched / %d total strings)\n",
			repo.GetFullName(), repoStringRatio, repoMatchedStrings, repoTotalStrings)

		// If we got any matches, write them to a local file in the same directory
		// as this main.go. Filenames like "owner-repo-matches.log".
		if len(allMatches) > 0 {
			reportFile := strings.ReplaceAll(repo.GetFullName(), "/", "-") + "-matches.log"
			f, err := os.Create(reportFile)
			if err != nil {
				log.Printf("Error creating report file %s: %v", reportFile, err)
				continue
			}

			defer f.Close()

			// Write out each match
			for _, m := range allMatches {
				line := fmt.Sprintf("%s:%d -> identifier=%s; literal=%q\n",
					m.File, m.LineNumber, m.Identifier, m.Literal)
				_, _ = f.WriteString(line)
			}

			log.Printf("Wrote %d matches for %s to %s\n",
				len(allMatches), repo.GetFullName(), reportFile)
		}
	}

	// Print ratio of “strings that contained an identifier” to “total strings seen”
	overallRatio := 0.0
	if totalStrings > 0 {
		overallRatio = float64(matchedStrings) / float64(totalStrings)
	}
	log.Printf("Overall identifier match ratio: %.4f (%d matched / %d total)\n",
		overallRatio, matchedStrings, totalStrings)
}

// cloneRepo does a shallow clone
func cloneRepo(gitURL, dest string) error {
	log.Printf("Cloning %s into %s", gitURL, dest)
	cmd := exec.Command("git", "clone", "--depth=1", gitURL, dest)
	return cmd.Run()
}

// analyzeFileWithLines parses a single Go file, counts total lines,
// gathers all top-level identifiers, checks each string literal
// to see if it contains any of those identifiers, and returns match details.
func analyzeFileWithLines(filePath string) (int, int, int, []matchInfo) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, parser.AllErrors)
	if err != nil {
		// Could not parse the file
		return 0, 0, 0, nil
	}

	// 1) Determine total line count
	fileObj := fset.File(node.Pos())
	if fileObj == nil {
		// If something is off or we got no file, just return zeros
		return 0, 0, 0, nil
	}
	totalLines := fileObj.LineCount()

	// 2) Gather all top-level identifiers: functions, types, variables, constants
	var identifiers []string
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			identifiers = append(identifiers, d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					identifiers = append(identifiers, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						identifiers = append(identifiers, n.Name)
					}
				}
			}
		}
	}

	stringLines := 0
	matchedStrs := 0
	matches := make([]matchInfo, 0)

	// 3) Inspect AST for string literals
	ast.Inspect(node, func(n ast.Node) bool {
		basicLit, ok := n.(*ast.BasicLit)
		if !ok || basicLit.Kind != token.STRING {
			return true
		}

		// Clean up the string literal (remove quotes/backticks)
		literalText := strings.Trim(basicLit.Value, "`\"")

		// Global total strings count
		totalStrings++
		stringLines++

		for _, id := range identifiers {
			if containsIdentifier(literalText, id) {
				// Global match count
				matchedStrings++
				matchedStrs++

				// Store match details (file path, line number, etc.)
				pos := fset.Position(basicLit.Pos())
				matches = append(matches, matchInfo{
					File:       filePath,
					LineNumber: pos.Line,
					Identifier: id,
					Literal:    literalText,
				})
				// Break after first successful match to avoid double-counting
				break
			}
		}
		return true
	})

	return totalLines, stringLines, matchedStrs, matches
}

// containsIdentifier returns true if `literal` contains `id` as a separate “word”
// and is NOT preceded directly by '%' or '\'.
func containsIdentifier(literal, id string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\b`)
	matches := re.FindAllStringIndex(literal, -1)
	for _, m := range matches {
		start := m[0]
		if start > 0 {
			prev := literal[start-1]
			if prev == '%' || prev == '\\' {
				continue
			}
		}
		return true
	}
	return false
}
