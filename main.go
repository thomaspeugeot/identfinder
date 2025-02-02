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
	"unicode"

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

		// CHANGE #1: Check if directory already exists. If it doesn't exist, clone.
		if _, statErr := os.Stat(tmpDir); os.IsNotExist(statErr) {
			if err := cloneRepo(repo.GetCloneURL(), tmpDir); err != nil {
				log.Printf("Error cloning %s: %v", repo.GetFullName(), err)
				continue
			}
		} else {
			log.Printf("Directory %q already exists, skipping clone", tmpDir)
		}

		// CHANGE #2: Remove the call to defer os.RemoveAll(tmpDir)
		// (so the directory is not removed after processing).

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

		// Write matches to a file if any
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

	return totalLines, v.stringCount, v.matchCount, v.matches
}

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

// -------------------------------------------------------------------
// Scope Tracking Visitor
// -------------------------------------------------------------------

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
		scopeStack: []*scope{
			{names: make(map[string]struct{})}, // global scope
		},
	}
}

func (v *scopeVisitor) pushScope() {
	v.scopeStack = append(v.scopeStack, &scope{names: make(map[string]struct{})})
}

func (v *scopeVisitor) popScope() {
	v.scopeStack = v.scopeStack[:len(v.scopeStack)-1]
}

func (v *scopeVisitor) addName(name string) {
	top := v.scopeStack[len(v.scopeStack)-1]
	top.names[name] = struct{}{}
}

func (v *scopeVisitor) inScope() []string {
	// gather all names from all active scopes
	var results []string
	for _, s := range v.scopeStack {
		for n := range s.names {
			results = append(results, n)
		}
	}
	return results
}

func (v *scopeVisitor) Visit(n ast.Node) ast.Visitor {
	switch node := n.(type) {

	case *ast.File:
		// We'll walk node.Decls anyway, so just return v
		return v

	case *ast.FuncDecl:
		// Push a new scope for the function
		v.pushScope()
		// Add the function name itself
		v.addName(node.Name.Name)
		// Add function parameters
		if node.Type.Params != nil {
			for _, param := range node.Type.Params.List {
				for _, pName := range param.Names {
					v.addName(pName.Name)
				}
			}
		}
		// Add function results (if they are named)
		if node.Type.Results != nil {
			for _, result := range node.Type.Results.List {
				for _, rName := range result.Names {
					v.addName(rName.Name)
				}
			}
		}
		// Walk the function body
		if node.Body != nil {
			ast.Walk(v, node.Body)
		}
		v.popScope()
		// Return nil so we don’t re-walk
		return nil

	case *ast.BlockStmt:
		// Push a block scope
		v.pushScope()
		for _, stmt := range node.List {
			ast.Walk(v, stmt)
		}
		v.popScope()
		return nil

	case *ast.AssignStmt:
		// For short variable declarations x := expr
		if node.Tok.String() == ":=" {
			for _, lh := range node.Lhs {
				if ident, ok := lh.(*ast.Ident); ok {
					v.addName(ident.Name)
				}
			}
		}
		return v

	case *ast.DeclStmt:
		// Local var/const/type declarations
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
		// Check string literals
		if node.Kind == token.STRING {
			v.stringCount++
			v.checkString(node)
		}
	}
	return v
}

func (v *scopeVisitor) checkString(basicLit *ast.BasicLit) {
	literalText := strings.Trim(basicLit.Value, "`\"")
	linePos := v.fset.Position(basicLit.Pos()).Line

	// Check each in-scope identifier
	names := v.inScope()
	for _, name := range names {
		if containsIdentifier(literalText, name) {
			// Found a valid match
			v.matchCount++
			matchedStrings++
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
			// We stop at the first matching identifier to avoid “double counting.”
			break
		}
	}

	// Also increment global total string count
	totalStrings++
}

// containsIdentifier returns true if `id` appears in `literal` such that it’s
// “surrounded by spaces, quotes, or punctuation” and NOT preceded directly
// by '%' or '\'. This avoids partial matches like “r” in “form-urlencoded”.
func containsIdentifier(literal, id string) bool {
	if id == "" {
		return false
	}

	searchStart := 0
	for {
		idx := strings.Index(literal[searchStart:], id)
		if idx == -1 {
			break
		}
		// Absolute position in the literal
		pos := searchStart + idx
		end := pos + len(id) - 1

		// Check preceding char
		if pos > 0 {
			prev := rune(literal[pos-1])
			// If it's '\' or '%', skip
			if prev == '\\' || prev == '%' {
				searchStart = pos + len(id)
				continue
			}
			// Must be a boundary if it's not start-of-string
			if !isBoundary(prev) {
				searchStart = pos + len(id)
				continue
			}
		}
		// Check following char
		if end < len(literal)-1 {
			next := rune(literal[end+1])
			if !isBoundary(next) {
				searchStart = pos + 1
				continue
			}
		}
		return true
	}
	return false
}

// isBoundary returns true if r is considered a “boundary” character
// (space, punctuation, quote, etc.) but not \ or %.
func isBoundary(r rune) bool {
	// Allowed boundary set: whitespace or punctuation/marks, etc.
	// You can customize the exact set. For instance, you could test
	// if it's a letter or digit to exclude it. Here we treat
	// any “space” or “punct” as a boundary.
	if unicode.IsSpace(r) {
		return true
	}
	if unicode.IsPunct(r) || unicode.IsSymbol(r) {
		// But exclude backslash and percent from boundary
		if r == '%' || r == '\\' {
			return false
		}
		return true
	}
	// You might also consider `unicode.IsMark(r)` depending on your needs.
	return false
}
