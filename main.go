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

func main() {
	var (
		minStars   int
		maxStars   int
		maxResults int
	)
	flag.IntVar(&minStars, "stars", 1000, "Minimum number of stars")
	flag.IntVar(&maxStars, "maxstars", 5000, "Maximum number of stars")
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

		for _, fpath := range goFiles {
			lines, strLines, matchedStrs := analyzeFileWithLines(fpath)
			totalLines += lines
			stringLines += strLines
			repoTotalStrings += strLines
			repoMatchedStrings += matchedStrs
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

func analyzeFileWithLines(filePath string) (int, int, int) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, parser.AllErrors)
	if err != nil {
		// could not parse the file
		return 0, 0, 0
	}

	totalLines := 0
	stringLines := 0
	matchedStrs := 0

	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		// Increment total lines
		pos := fset.Position(n.Pos())
		if pos.IsValid() {
			totalLines++
		}

		basicLit, ok := n.(*ast.BasicLit)
		if !ok || basicLit.Kind != token.STRING {
			return true
		}

		literalText := strings.Trim(basicLit.Value, "`\"")
		totalStrings++
		stringLines++

		ast.Inspect(n, func(inner ast.Node) bool {
			if funcDecl, ok := inner.(*ast.FuncDecl); ok {
				funcName := funcDecl.Name.Name
				if containsIdentifier(literalText, funcName) {
					matchedStrings++
					matchedStrs++
				}
			}
			return true
		})
		return true
	})

	return totalLines, stringLines, matchedStrs
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
