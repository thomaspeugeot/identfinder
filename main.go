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
		maxResults int
	)
	flag.IntVar(&minStars, "stars", 1000, "Minimum number of stars")
	flag.IntVar(&maxResults, "max", 5, "Max number of repositories to process")
	flag.Parse()

	ctx := context.Background()
	tc := oauth2.NewClient(ctx, nil)

	client := github.NewClient(tc)

	// Search for Go repos with >= minStars stars
	query := fmt.Sprintf("language:Go stars:>=%d", minStars)
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

		for _, fpath := range goFiles {
			analyzeFile(fpath)
		}
	}

	// Print ratio of “strings that contained an identifier” to “total strings seen”
	ratio := 0.0
	if totalStrings > 0 {
		ratio = float64(matchedStrings) / float64(totalStrings)
	}
	log.Printf("Detected ratio: %.4f (%d matched / %d total)\n",
		ratio, matchedStrings, totalStrings)
}

// cloneRepo does a shallow clone
func cloneRepo(gitURL, dest string) error {
	log.Printf("Cloning %s into %s", gitURL, dest)
	cmd := exec.Command("git", "clone", "--depth=1", gitURL, dest)
	return cmd.Run()
}

func analyzeFile(filePath string) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, parser.AllErrors)
	if err != nil {
		// could not parse the file
		return
	}

	// For each function declaration, gather identifiers (func + params)
	// Then look for string literals in that function's body,
	// increment counters if a match is found.
	ast.Inspect(node, func(n ast.Node) bool {
		funcDecl, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}

		var identifiers []string
		funcName := funcDecl.Name.Name
		identifiers = append(identifiers, funcName)

		if funcDecl.Type.Params != nil {
			for _, field := range funcDecl.Type.Params.List {
				for _, name := range field.Names {
					identifiers = append(identifiers, name.Name)
				}
			}
		}

		if funcDecl.Body == nil {
			return true
		}

		ast.Inspect(funcDecl.Body, func(b ast.Node) bool {
			basicLit, ok := b.(*ast.BasicLit)
			if !ok || basicLit.Kind != token.STRING {
				return true
			}
			// Strip quotes from the literal value
			literalText := strings.Trim(basicLit.Value, "`\"")

			totalStrings++ // We encountered one string literal overall

			// We only count this string once if it references any identifier
			foundAny := false
			for _, id := range identifiers {
				if containsIdentifier(literalText, id) {
					if !foundAny {
						matchedStrings++
						foundAny = true
					}
					// Print a debug message about the match
					fmt.Printf("[MATCH] %s: Found string %q containing identifier %q (Func: %s)\n",
						filePath, literalText, id, funcName)
				}
			}
			return true
		})

		return false
	})

}

// containsIdentifier returns true if `literal` contains `id` as a separate “word”
// and is NOT preceded directly by '%' or '\'.
//
// We still do a word-boundary match using \b on both sides. After finding
// a potential match, we check the character before that match to ensure it
// is not '%' or '\'.
func containsIdentifier(literal, id string) bool {
	// Regex: \b id \b
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\b`)
	matches := re.FindAllStringIndex(literal, -1)
	for _, m := range matches {
		start := m[0]
		// If there's a preceding character, it must NOT be '%' or '\'
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
