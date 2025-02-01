package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v48/github"
	"golang.org/x/oauth2"
)

// Global counters for ratio calculation
var (
	totalStrings   int
	matchedStrings int
)

// matchInfo holds details about each matched string.
type matchInfo struct {
	File       string
	LineNumber int
	Identifier string
	StringText string
	EntireLine string
}

func main() {
	var (
		minStars   int
		maxStars   int
		maxResults int
	)
	flag.IntVar(&minStars, "stars", 1000, "Minimum number of stars")
	flag.IntVar(&maxStars, "maxstars", 9000, "Maximum number of stars")
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
		// defer os.RemoveAll(tmpDir)

		goFiles := gatherGoFiles(tmpDir)

		var totalLines, stringLines, repoTotalStrings, repoMatchedStrings int
		var allMatches []matchInfo

		for _, fpath := range goFiles {
			lines, strLines, matchedStrs, matches := analyzeFileWithLines(fpath)
			totalLines += lines
			stringLines += strLines
			repoTotalStrings += strLines
			repoMatchedStrings += matchedStrs
			allMatches = append(allMatches, matches...)
		}

		// Calculate ratios for the repository
		repoLineRatio := 0.0
		if totalLines > 0 {
			repoLineRatio = float64(stringLines) / float64(totalLines)
		}

		repoStringRatio := 0.0
		if repoTotalStrings > 0 {
			repoStringRatio = float64(repoMatchedStrings) / float64(repoTotalStrings)
		}

		log.Printf("Repository %s: String-to-total-line ratio: %.4f (%d string lines / %d total lines)\n",
			repo.GetFullName(), repoLineRatio, stringLines, totalLines)
		log.Printf("Repository %s: Matched-strings-to-total-strings ratio: %.4f (%d matched / %d total strings)\n",
			repo.GetFullName(), repoStringRatio, repoMatchedStrings, repoTotalStrings)

		// If we have matches, write them to a local file
		if len(allMatches) > 0 {
			reportFile := strings.ReplaceAll(repo.GetFullName(), "/", "-") + "-matches.log"
			f, err := os.Create(reportFile)
			if err != nil {
				log.Printf("Error creating report file %s: %v", reportFile, err)
				continue
			}
			defer f.Close()

			for _, m := range allMatches {
				line := fmt.Sprintf(
					"%s:%d -> identifier=%s; string=%q; entire_line=%q\n",
					m.File, m.LineNumber, m.Identifier, m.StringText, m.EntireLine,
				)
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

// gatherGoFiles recursively gathers all .go files under the specified root.
func gatherGoFiles(root string) []string {
	var goFiles []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	return goFiles
}

// cloneRepo does a shallow clone
func cloneRepo(gitURL, dest string) error {
	log.Printf("Cloning %s into %s", gitURL, dest)
	cmd := exec.Command("git", "clone", "--depth=1", gitURL, dest)
	return cmd.Run()
}

// analyzeFileWithLines parses a single Go file, counts total lines, and inspects
// string literals to see if they contain any in-scope identifiers. Returns
// a slice of matchInfo (including the entire line from the source file).
func analyzeFileWithLines(filePath string) (int, int, int, []matchInfo) {
	// Read all lines so we can log the "entire line" for each match
	srcLines, err := readFileLines(filePath)
	if err != nil || len(srcLines) == 0 {
		return 0, 0, 0, nil
	}

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, parser.AllErrors)
	if err != nil {
		return 0, 0, 0, nil
	}

	// Find total line count from token.File
	fileObj := fset.File(node.Pos())
	if fileObj == nil {
		return 0, 0, 0, nil
	}
	totalLines := fileObj.LineCount()

	// Our visitor will track scopes (function params, local vars, etc.)
	v := newScopeVisitor(fset, filePath, srcLines)
	ast.Walk(v, node)

	// Add to global counters
	return totalLines, v.stringCount, v.matchCount, v.matches
}

// -------------------------------------------------------------------
// Scope Tracking Visitor
// -------------------------------------------------------------------

// scopeVisitor holds state for AST traversal.
// - We keep a stack of scopes, each scope holds a set of names in scope.
type scopeVisitor struct {
	fset        *token.FileSet
	filePath    string
	srcLines    []string
	matches     []matchInfo
	stringCount int
	matchCount  int

	scopeStack []*scope
}

// scope is a collection of in-scope identifier names
type scope struct {
	names map[string]struct{}
}

func newScopeVisitor(fset *token.FileSet, filePath string, srcLines []string) *scopeVisitor {
	return &scopeVisitor{
		fset:     fset,
		filePath: filePath,
		srcLines: srcLines,
		// Start with a global "empty" scope
		scopeStack: []*scope{{names: make(map[string]struct{})}},
	}
}

// Push a new scope on the stack
func (v *scopeVisitor) pushScope() {
	v.scopeStack = append(v.scopeStack, &scope{
		names: make(map[string]struct{}),
	})
}

// Pop the current scope
func (v *scopeVisitor) popScope() {
	v.scopeStack = v.scopeStack[:len(v.scopeStack)-1]
}

// Add a name to the top scope
func (v *scopeVisitor) addName(name string) {
	top := v.scopeStack[len(v.scopeStack)-1]
	top.names[name] = struct{}{}
}

// inScope returns all names from the top scopes combined
func (v *scopeVisitor) inScope() []string {
	var results []string
	for _, s := range v.scopeStack {
		for n := range s.names {
			results = append(results, n)
		}
	}
	return results
}

// Visit is the main entry point for our AST walk.
func (v *scopeVisitor) Visit(n ast.Node) ast.Visitor {
	switch node := n.(type) {
	case *ast.File:
		// File-level declarations (types, funcs, vars, consts)
		// go through them below via ast.Walk, so no immediate push/pop here
		return v

	case *ast.FuncDecl:
		// We push a scope for the function.
		v.pushScope()
		// Add the function name
		v.addName(node.Name.Name)
		// Add parameter names
		for _, p := range node.Type.Params.List {
			for _, paramName := range p.Names {
				v.addName(paramName.Name)
			}
		}
		// Walk the function's body with a child visitor
		ast.Walk(v, node.Body)
		// After traversing the body, pop the scope
		v.popScope()
		// Return nil so we don't re-traverse the body
		return nil

	case *ast.BlockStmt:
		// We push a scope for each block
		v.pushScope()
		for _, stmt := range node.List {
			ast.Walk(v, stmt)
		}
		v.popScope()
		// Return nil so we don't re-traverse
		return nil

	case *ast.AssignStmt:
		// For short variable declarations: `x := 123`
		// we add these names to the top scope
		if node.Tok.String() == ":=" {
			for _, expr := range node.Lhs {
				if ident, ok := expr.(*ast.Ident); ok {
					v.addName(ident.Name)
				}
			}
		}
		return v

	case *ast.DeclStmt:
		// This is a local declaration like `var x int` or `const y = ...`
		if gen, ok := node.Decl.(*ast.GenDecl); ok {
			for _, spec := range gen.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						v.addName(n.Name)
					}
				case *ast.TypeSpec:
					v.addName(s.Name.Name)
				}
			}
		}
		return v

	case *ast.BasicLit:
		// If it's a string, check for in-scope references
		if node.Kind == token.STRING {
			v.stringCount++
			v.checkString(node)
		}
	}

	return v
}

// checkString checks the given string literal for any in-scope identifiers.
func (v *scopeVisitor) checkString(basicLit *ast.BasicLit) {
	literalText := strings.Trim(basicLit.Value, "`\"")
	linePos := v.fset.Position(basicLit.Pos()).Line

	// Gather all names in scope
	names := v.inScope()

	// For each name, check if the string literal contains it
	// (and skip if preceded by '%' or '\').
	for _, name := range names {
		if containsIdentifier(literalText, name) {
			// Once we find a match, record it and stop to avoid double counting.
			v.matchCount++
			matchedStrings++
			// We'll log the entire line from source
			entireLine := ""
			if linePos-1 >= 0 && linePos-1 < len(v.srcLines) {
				entireLine = v.srcLines[linePos-1]
			}
			v.matches = append(v.matches, matchInfo{
				File:       v.filePath,
				LineNumber: linePos,
				Identifier: name,
				StringText: literalText,
				EntireLine: entireLine,
			})
			break
		}
	}

	// Also increment the global total string count
	totalStrings++
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

// readFileLines returns a slice of all lines in the given file.
func readFileLines(filePath string) ([]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// containsIdentifier checks if `literal` contains `id` as a substring
// that is not directly preceded by '%' or '\'.
func containsIdentifier(literal, id string) bool {
	idx := 0
	for {
		loc := strings.Index(literal[idx:], id)
		if loc < 0 {
			break
		}
		absPos := idx + loc

		// Check preceding char
		if absPos > 0 {
			prev := literal[absPos-1]
			if prev == '%' || prev == '\\' {
				// skip this occurrence, continue searching
				idx = absPos + len(id)
				continue
			}
		}
		return true
	}
	return false
}
