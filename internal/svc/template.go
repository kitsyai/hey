package svc

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// vars holds the template substitutions for a command (see docs, "Templating").
type vars struct {
	bin, data, user, password, pwfile, model string
	port                                     int
}

func (v vars) replacer() *strings.Replacer {
	return strings.NewReplacer(
		"{bin}", v.bin,
		"{data}", v.data,
		"{port}", fmt.Sprintf("%d", v.port),
		"{user}", v.user,
		"{password}", v.password,
		"{pwfile}", v.pwfile,
		"{model}", v.model,
	)
}

// expand parses a command template into argv: it tokenizes with double-quote
// handling FIRST, then substitutes template variables into each token, so
// values that contain spaces (Windows paths) never split an argument. The
// first token is treated as the executable and, on Windows, gets a ".exe"
// suffix resolved if the bare path does not exist.
func expand(tmpl string, v vars) ([]string, error) {
	toks, err := tokenize(tmpl)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty command template")
	}
	rep := v.replacer()
	argv := make([]string, len(toks))
	for i, t := range toks {
		argv[i] = rep.Replace(t)
	}
	argv[0] = resolveExe(argv[0])
	return argv, nil
}

// tokenize splits s on whitespace, honoring double quotes so `-k ""` yields an
// empty argument. Backslashes are always literal — templates carry Windows
// paths, and no shell-style quote escaping is needed.
func tokenize(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	inTok, inQuote := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			inTok = true
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			if inTok {
				toks = append(toks, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in command %q", s)
	}
	if inTok {
		toks = append(toks, cur.String())
	}
	return toks, nil
}

// resolveExe returns path unchanged, except on Windows where, if path does not
// exist but path+".exe" does, the .exe form is returned. This lets one
// manifest ("{bin}/initdb") work on every platform.
func resolveExe(path string) string {
	if runtime.GOOS != "windows" || path == "" {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if _, err := os.Stat(path + ".exe"); err == nil {
		return path + ".exe"
	}
	return path
}
