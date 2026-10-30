package views

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"strings"
)

// templateFiles embeds the HTML templates compiled into the binary. Keeping them
// as .html files (rather than raw Go string constants) restores syntax
// highlighting, editor tooling, and template linting.
//
//go:embed templates/*.html
var templateFiles embed.FS

// mustTemplates builds a template set from the named template files, concatenated
// in the given order. Concatenation (rather than template.ParseFS, which makes
// each file an associated template) preserves the original composition: every
// file is either a `{{define}}`-only partial or top-level layout markup, and the
// order determines the page skeleton. It panics on a missing file or a parse
// error, matching the previous template.Must behaviour at package init.
func mustTemplates(name string, files ...string) *template.Template {
	return template.Must(template.New(name).Funcs(templateFuncs).Parse(readTemplates(files...)))
}

// readTemplates concatenates the contents of the named embedded template files.
func readTemplates(files ...string) string {
	var b strings.Builder
	for _, file := range files {
		data, err := templateFiles.ReadFile("templates/" + file)
		if err != nil {
			panic(fmt.Sprintf("views: read template %q: %v", file, err))
		}
		// Many editors add a trailing newline that the former Go string
		// constants did not have; strip it so composition stays byte-exact.
		b.Write(bytes.TrimSuffix(data, []byte("\n")))
	}
	return b.String()
}
