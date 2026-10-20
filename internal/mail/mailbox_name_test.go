package mail

import "testing"

func TestResolveMailboxName(t *testing.T) {
	inboxTree := []string{"INBOX", "INBOX.Sent", "INBOX.Archive"}
	tests := []struct {
		name       string
		configured string
		existing   []string
		delim      rune
		prefix     string
		want       string
		wantExists bool
	}{
		{"top-level folder exists", "Junk", []string{"INBOX", "Junk"}, '/', "", "Junk", true},
		{"case-insensitive match", "Junk", []string{"INBOX", "junk"}, '/', "", "junk", true},
		{"INBOX hierarchy, folder exists", "Archive", inboxTree, '.', "", "INBOX.Archive", true},
		{"INBOX hierarchy, folder missing", "Junk", inboxTree, '.', "", "INBOX.Junk", false},
		{"namespace prefix from NAMESPACE", "Junk", []string{"INBOX"}, '.', "INBOX.", "INBOX.Junk", false},
		{"already prefixed", "INBOX.Junk", inboxTree, '.', "INBOX.", "INBOX.Junk", false},
		{"flat server, folder missing", "Junk", []string{"INBOX"}, '/', "", "Junk", false},
		{"inbox itself", "inbox", inboxTree, '.', "", "INBOX", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, exists := resolveMailboxName(tt.configured, tt.existing, tt.delim, tt.prefix)
			if got != tt.want || exists != tt.wantExists {
				t.Fatalf("resolveMailboxName(%q) = %q,%v want %q,%v", tt.configured, got, exists, tt.want, tt.wantExists)
			}
		})
	}
}
