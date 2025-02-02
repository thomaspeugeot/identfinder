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

// -------------------------------------------------------------
// Global counters for ratio calculation
// -------------------------------------------------------------
var (
	totalStrings   int
	matchedStrings int
)

// matchInfo holds details about each matched string literal.
type matchInfo struct {
	File       string
	LineNumber int
	Identifier string
	StringText string
	EntireLine string
}

func main() {
	// -------------------------------------------
	// Redirect log output to "result.log"
	// -------------------------------------------
	f, err := os.Create("result.log")
	if err != nil {
		log.Fatalf("Error creating result.log: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)

	// Define flags
	var (
		minStars   int
		maxStars   int
		maxResults int
	)

	flag.IntVar(&minStars, "stars", 1000, "Minimum number of stars for search")
	flag.IntVar(&maxStars, "maxstars", 9000, "Maximum number of stars for search")
	flag.IntVar(&maxResults, "max", 5, "Max number of repositories to process (when searching)")

	// Parse flags
	flag.Parse()

	// If the user provided arguments after the flags, interpret them as GitHub repos.
	args := flag.Args()

	if len(args) > 0 {
		// --------------------------------------------------------------------
		// CASE 1: The user specified one or more repos directly in the args
		// --------------------------------------------------------------------
		log.Println("Positional arguments detected. Skipping GitHub search.")
		for _, repoPath := range args {
			log.Printf("Analyzing requested GitHub repo: %s\n", repoPath)
			analyzeSingleGitHubRepo(repoPath)
		}

	} else {
		// --------------------------------------------------------------------
		// CASE 2: No positional args => Use the GitHub search logic
		// --------------------------------------------------------------------
		ctx := context.Background()
		tc := oauth2.NewClient(ctx, nil)
		client := github.NewClient(tc)

		// Build the search query with star range
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

			// Construct local directory name for cloning (if needed)
			tmpDir := fmt.Sprintf("repo-%s", strings.ReplaceAll(repo.GetFullName(), "/", "-"))

			// If directory doesn't exist, clone
			if _, statErr := os.Stat(tmpDir); os.IsNotExist(statErr) {
				if err := cloneRepo(repo.GetCloneURL(), tmpDir); err != nil {
					log.Printf("Error cloning %s: %v", repo.GetFullName(), err)
					continue
				}
			} else {
				log.Printf("Directory %q already exists, skipping clone", tmpDir)
			}

			// Analyze the local repo (already cloned or existing)
			analyzeLocalRepo(tmpDir, repo.GetFullName())
		}
	}

	// At the end, print overall ratio of “strings that contained an identifier” to “total strings seen”
	overallRatio := 0.0
	if totalStrings > 0 {
		overallRatio = float64(matchedStrings) / float64(totalStrings)
	}
	log.Printf("Overall identifier match ratio: %.4f (%d matched / %d total)\n",
		overallRatio, matchedStrings, totalStrings)
}

// -------------------------------------------------------------
// GITHUB REPO CLONING & ANALYSIS
// -------------------------------------------------------------

// analyzeSingleGitHubRepo takes a GitHub repo path like "github.com/user/repo".
// If the local clone folder doesn't exist, it clones from "https://github.com/user/repo.git"
// into "repo-github.com-user-repo". Then analyzes it.
func analyzeSingleGitHubRepo(repoPath string) {
	// Build a clone URL, e.g. "https://github.com/user/repo.git"
	cloneURL := "https://" + strings.TrimSuffix(repoPath, ".git") + ".git"

	// Local folder name, e.g. "repo-github.com-user-repo"
	localDir := fmt.Sprintf("repo-%s", strings.ReplaceAll(repoPath, "/", "-"))

	// If directory doesn't exist, clone
	if _, err := os.Stat(localDir); os.IsNotExist(err) {
		log.Printf("Cloning %s into %s\n", cloneURL, localDir)
		if err := cloneRepo(cloneURL, localDir); err != nil {
			log.Printf("Error cloning %s: %v", cloneURL, err)
			return
		}
	} else {
		log.Printf("Directory %q already exists, skipping clone", localDir)
	}

	// Analyze that local directory
	analyzeLocalRepo(localDir, repoPath)
}

// cloneRepo does a shallow clone from the given gitURL into dest
func cloneRepo(gitURL, dest string) error {
	log.Printf("Cloning %s into %s", gitURL, dest)
	cmd := exec.Command("git", "clone", "--depth=1", gitURL, dest)
	return cmd.Run()
}

// -------------------------------------------------------------
// LOCAL REPO ANALYSIS
// -------------------------------------------------------------

// analyzeLocalRepo walks all Go files in repoDir, analyzes them, prints stats,
// and writes match logs to a file named "<repoName>-matches.log" if there are matches.
func analyzeLocalRepo(repoDir, repoName string) {
	goFiles := gatherGoFiles(repoDir)

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
		repoName, repoLineRatio, stringLines, totalLines)
	log.Printf("Repository %s: Matched-strings-to-total-strings ratio: %.4f (%d matched / %d total strings)\n",
		repoName, repoStringRatio, repoMatchedStrings, repoTotalStrings)

	// If matches found, write them to a log file
	if len(allMatches) > 0 {
		logFile := strings.ReplaceAll(repoName, "/", "-") + "-matches.log"
		f, err := os.Create(logFile)
		if err != nil {
			log.Printf("Error creating report file %s: %v", logFile, err)
			return
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
			len(allMatches), repoName, logFile)
	}
}

// gatherGoFiles recursively gathers all .go files under the specified root directory.
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

// -------------------------------------------------------------
// FILE-LEVEL ANALYSIS
// -------------------------------------------------------------

// analyzeFileWithLines parses a single Go file, counts total lines, and inspects
// string literals to see if they contain any in-scope identifiers. Returns
// line counts, match counts, and a slice of matchInfo (including the entire line).
func analyzeFileWithLines(filePath string) (int, int, int, []matchInfo) {
	// Read all lines so we can log the entire line if there's a match
	srcLines, err := readFileLines(filePath)
	if err != nil || len(srcLines) == 0 {
		return 0, 0, 0, nil
	}

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, parser.AllErrors)
	if err != nil {
		return 0, 0, 0, nil
	}

	// Determine total line count from the token.File
	fileObj := fset.File(node.Pos())
	if fileObj == nil {
		return 0, 0, 0, nil
	}
	totalLines := fileObj.LineCount()

	// Use our scopeVisitor to track local variables, function parameters, etc.
	v := newScopeVisitor(fset, filePath, srcLines)
	ast.Walk(v, node)

	// v.stringCount: how many string literals in this file
	// v.matchCount: how many matched an in-scope identifier
	// v.matches: the details of each match
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

// -------------------------------------------------------------
// SCOPE VISITOR & IDENTIFIER MATCHING
// -------------------------------------------------------------

// scopeVisitor holds state for AST traversal, including a stack of scopes
// that track which identifiers (vars, params, etc.) are in scope.
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

// addName adds an identifier to the top scope in the stack
func (v *scopeVisitor) addName(name string) {
	top := v.scopeStack[len(v.scopeStack)-1]
	top.names[name] = struct{}{}
}

// inScope returns a list of all names in all active scopes
func (v *scopeVisitor) inScope() []string {
	var results []string
	for _, s := range v.scopeStack {
		for n := range s.names {
			results = append(results, n)
		}
	}
	return results
}

// Visit implements the ast.Visitor interface
func (v *scopeVisitor) Visit(n ast.Node) ast.Visitor {
	switch node := n.(type) {

	case *ast.File:
		// We'll walk node.Decls anyway, so just return v
		return v

	case *ast.FuncDecl:
		// Push a scope for the function
		v.pushScope()
		// Add the function name
		v.addName(node.Name.Name)
		// Add function parameters
		if node.Type.Params != nil {
			for _, param := range node.Type.Params.List {
				for _, pName := range param.Names {
					v.addName(pName.Name)
				}
			}
		}
		// Add named result parameters
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
		// Return nil so we don't re-walk
		return nil

	case *ast.BlockStmt:
		// Push a scope for each block
		v.pushScope()
		for _, stmt := range node.List {
			ast.Walk(v, stmt)
		}
		v.popScope()
		return nil

	case *ast.AssignStmt:
		// For short variable declarations: x := 123
		if node.Tok.String() == ":=" {
			for _, lh := range node.Lhs {
				if ident, ok := lh.(*ast.Ident); ok {
					v.addName(ident.Name)
				}
			}
		}
		return v

	case *ast.DeclStmt:
		// For local var/const/type declarations
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
		// If it's a string, check for in-scope identifiers
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

	// Check each in-scope identifier
	names := v.inScope()
	for _, name := range names {
		if containsIdentifier(literalText, name) {
			v.matchCount++
			matchedStrings++

			// Grab entire source line
			entireLine := ""
			if linePos-1 >= 0 && linePos-1 < len(v.srcLines) {
				entireLine = v.srcLines[linePos-1]
			}
			// Record match
			v.matches = append(v.matches, matchInfo{
				File:       v.filePath,
				LineNumber: linePos,
				Identifier: name,
				StringText: literalText,
				EntireLine: entireLine,
			})
			// Stop after first matching identifier so we don't double-count
			break
		}
	}

	// Also increment the global total string count
	totalStrings++
}

// containsIdentifier returns true if `id` appears in `literal` such that it’s
// “surrounded by boundary characters” (space, punctuation, quotes, etc.)
// and NOT preceded directly by '%' or '\'.
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
		pos := searchStart + idx
		end := pos + len(id) - 1

		// Check preceding char (if not start of string)
		if pos > 0 {
			prev := rune(literal[pos-1])
			// If it's '\' or '%', skip this occurrence
			if prev == '\\' || prev == '%' {
				searchStart = pos + len(id)
				continue
			}
			// Otherwise, must be boundary
			if !isBoundary(prev) {
				searchStart = pos + len(id)
				continue
			}
		}
		// Check following char (if not end of string)
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

// isBoundary returns true if r is considered a “boundary” character:
// whitespace, punctuation, or symbol (but excluding backslash and percent).
func isBoundary(r rune) bool {
	// Check for whitespace
	if unicode.IsSpace(r) {
		return true
	}
	// Check punctuation/symbol
	if unicode.IsPunct(r) || unicode.IsSymbol(r) {
		// Exclude '\' and '%'
		if r == '\\' || r == '%' {
			return false
		}
		return true
	}
	return false
}
