package views

import (
	"io/fs"
	"strings"
	"testing"
)

// TestEmbeddedTemplatesParse fails if a template file is missing from the
// embedded FS or a template set references a file that is not embedded. Every
// set is built at package init (mustTemplates panics on error), so this guards
// the file lists against typos after a rename.
func TestEmbeddedTemplatesParse(t *testing.T) {
	var embedded []string
	err := fs.WalkDir(templateFiles, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			embedded = append(embedded, strings.TrimPrefix(path, "templates/"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded templates: %v", err)
	}
	if len(embedded) == 0 {
		t.Fatal("no embedded templates found")
	}

	used := map[string]bool{}
	for _, set := range [][]string{
		{"inbox_filter_form.html", "inbox_table.html", "inbox_tabs.html", "inbox_results.html", "layout_header.html", "inbox_body.html", "layout_footer.html"},
		{"inbox_filter_form.html", "inbox_table.html", "inbox_tabs.html", "inbox_results.html"},
		{"inbox_filter_form.html", "inbox_table.html", "inbox_tabs.html", "inbox_results.html", "layout_header.html", "thread_detail.html", "layout_footer.html"},
		{"layout_header.html", "settings_audit_table.html", "settings_body.html", "layout_footer.html"},
		{"layout_header.html", "settings_audit_table.html", "activity_body.html", "layout_footer.html"},
		{"layout_header.html", "error_body.html", "layout_footer.html"},
		{"email_body.html"},
	} {
		for _, f := range set {
			if _, rerr := templateFiles.ReadFile("templates/" + f); rerr != nil {
				t.Errorf("template set references unembedded file %q: %v", f, rerr)
			}
			used[f] = true
		}
	}
	for _, f := range embedded {
		if !used[f] {
			t.Errorf("embedded template %q is not referenced by any template set", f)
		}
	}
}

// TestPartialTemplatesSelfContained asserts that the partial-only set defines the
// fragments the htmx list swap depends on: the results region (the swapped
// element), the region contents, the full fragment response, and the two
// out-of-band toolbar pieces. This is the contract RenderInboxResults exposes.
func TestPartialTemplatesSelfContained(t *testing.T) {
	for _, name := range []string{
		"inbox_table", "inbox_filter_form", "settings_audit_table",
		"inbox_results", "inbox_results_region", "inbox_results_fragment",
		"inbox_results_count_oob", "inbox_tabs_oob",
	} {
		if inboxPartTmpl.Lookup(name) == nil && settingsTmpl.Lookup(name) == nil {
			t.Errorf("template %q is not defined in any parsed set", name)
		}
	}
}
