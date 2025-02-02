# Go String Analyzer (identfinder)

A static analysis tool that examines Go repositories to find string literals containing in-scope identifiers. This tool helps identify potential string interpolation candidates and analyze string usage patterns in Go codebases.

## Features

- Analyzes Go repositories from GitHub based on star count range
- Supports direct repository URL input
- Performs recursive analysis of all Go files in a repository
- Tracks string literals that contain in-scope identifiers
- Generates detailed match reports for each repository
- Calculates various statistics including match ratios and line counts

## Installation

```bash
go get github.com/thomaspeugeot/identfinder
```

## Usage

### Basic Usage

```bash
# Analyze specific GitHub repositories
identfinder github.com/user/repo1 github.com/user/repo2

# Search and analyze repositories based on star count
identfinder -stars 1000 -maxstars 9000 -max 5
```

### Command Line Flags

- `-stars`: Minimum number of repository stars (default: 1000)
- `-maxstars`: Maximum number of repository stars (default: 9000)
- `-max`: Maximum number of repositories to process when searching (default: 5)

## Output

The tool generates several types of output:

1. `result.log`: Contains overall analysis results and progress information
2. `{repository-name}-matches.log`: Generated for each analyzed repository that contains matches, including:
   - File path and line number
   - Matched identifier
   - String literal content
   - Complete line of code

## How It Works

1. **Repository Collection**:
   - Either processes directly specified repositories
   - Or searches GitHub for Go repositories within the specified star range

2. **Code Analysis**:
   - Clones repositories locally (shallow clone)
   - Parses all Go files using Go's AST parser
   - Tracks variable scope and identifiers
   - Analyzes string literals for in-scope identifier matches

3. **Match Detection**:
   - Identifies string literals containing in-scope variables
   - Ensures matches are proper identifiers (not part of larger words)
   - Excludes format strings (%v) and escape sequences

## Example Output

```
Repository kubernetes/kubernetes: 
String-to-total-line ratio: 0.0234 (1234 string lines / 52650 total lines)
Matched-strings-to-total-strings ratio: 0.0856 (106 matched / 1238 total strings)

// In kubernetes-matches.log:
pkg/api/v1/pod/util.go:123 -> identifier=containerName; string="container %s not found"; entire_line='return fmt.Errorf("container %s not found", containerName)'
```

## Technical Details

The analyzer:
- Uses Go's `go/parser` and `go/ast` packages for code analysis
- Maintains a scope stack to track local variables, parameters, and other identifiers
- Performs boundary checking to ensure matched identifiers are complete words
- Handles various Go constructs including:
  - Function declarations and parameters
  - Variable declarations
  - Short variable declarations
  - Block scopes
  - Named return values

## Limitations

- Only performs shallow clones of repositories
- Does not analyze string literals in generated code
- May have false positives in complex string patterns
- Does not track struct field access or method calls

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

MIT
