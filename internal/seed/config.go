// Package seed loads declarative demo content and applies it through production IMAP,
// threading, and workflow code.
package seed

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wichert/minicrm/internal/config"
	"gopkg.in/yaml.v3"
)

// Status values accepted on a seed thread.
const (
	StatusNew        = "new"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusSpam       = "spam"
)

// Direction values accepted on a seed message.
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// Assignment sources. Default leaves assignment to the account's configured default assignee.
const (
	AssignManual  = "manual"
	AssignDefault = "default"
)

var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// File is the root of a seed YAML document.
type File struct {
	Accounts []string `yaml:"accounts"`
	Users    []User   `yaml:"users"`
	Settings Settings `yaml:"settings"`
	Threads  []Thread `yaml:"threads"`
	baseDir  string   `yaml:"-"`
}

// User is a pre-created assignee.
type User struct {
	Email       string   `yaml:"email"`
	DisplayName string   `yaml:"display_name"`
	Accounts    []string `yaml:"accounts"`
}

// Settings holds demo system and per-account defaults.
type Settings struct {
	Timezone         string            `yaml:"timezone"`
	DefaultAssignees map[string]string `yaml:"default_assignees"`
}

// Thread is one conversation to APPEND, ingest, and optionally transition.
type Thread struct {
	ID             string    `yaml:"id"`
	Account        string    `yaml:"account"`
	Customer       string    `yaml:"customer"`
	CustomerName   string    `yaml:"customer_name"`
	Subject        string    `yaml:"subject"`
	Status         string    `yaml:"status"`
	AssignedTo     string    `yaml:"assigned_to"`
	AssignedSource string    `yaml:"assigned_source"`
	Draft          string    `yaml:"draft"`
	Reopens        string    `yaml:"reopens"`
	Messages       []Message `yaml:"messages"`
}

// Message is one RFC 822 message belonging to a thread.
type Message struct {
	Direction string       `yaml:"direction"`
	From      string       `yaml:"from"`
	FromName  string       `yaml:"from_name"`
	Date      time.Time    `yaml:"date"`
	HTML      bool         `yaml:"html"`
	Body      string       `yaml:"body"`
	Attach    []Attachment `yaml:"attach"`
}

// Attachment is a file to include as a MIME part.
type Attachment struct {
	Filename    string `yaml:"filename"`
	ContentType string `yaml:"content_type"`
	File        string `yaml:"file"`
}

// LoadFile reads and validates a seed YAML document.
//
// baseDir is the directory relative attachment paths are resolved against. When empty,
// paths are resolved relative to the YAML file's directory.
func LoadFile(path, baseDir string, accounts []config.MailAccountConfig) (*File, error) {
	// #nosec G304 -- path is the operator-supplied -config flag, not request input
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed config %s: %w", path, err)
	}
	if baseDir == "" {
		baseDir = filepath.Dir(path)
	}
	f, err := Parse(raw, baseDir, accounts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse validates raw YAML against the configured mail accounts.
func Parse(raw []byte, baseDir string, accounts []config.MailAccountConfig) (*File, error) {
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse seed yaml: %w", err)
	}
	f.baseDir = baseDir
	if err := f.Validate(accounts); err != nil {
		return nil, err
	}
	return &f, nil
}

// BaseDir returns the directory attachment paths are resolved against.
func (f *File) BaseDir() string { return f.baseDir }

// Validate fails fast with a path-qualified error when the document cannot be seeded.
func (f *File) Validate(accounts []config.MailAccountConfig) error {
	byID := map[string]config.MailAccountConfig{}
	for _, acct := range accounts {
		byID[acct.ID] = acct
	}
	if len(f.Accounts) == 0 {
		return fmt.Errorf("accounts: at least one account id is required")
	}
	participating := map[string]struct{}{}
	for i, id := range f.Accounts {
		if _, ok := byID[id]; !ok {
			return fmt.Errorf("accounts[%d]: %q is not a configured mail account", i, id)
		}
		if _, dup := participating[id]; dup {
			return fmt.Errorf("accounts[%d]: duplicate account %q", i, id)
		}
		participating[id] = struct{}{}
	}

	users := map[string]User{}
	for i, u := range f.Users {
		email := strings.ToLower(strings.TrimSpace(u.Email))
		if email == "" || !strings.Contains(email, "@") {
			return fmt.Errorf("users[%d]: email %q is invalid", i, u.Email)
		}
		if strings.TrimSpace(u.DisplayName) == "" {
			return fmt.Errorf("users[%d] (%s): display_name is required", i, email)
		}
		if len(u.Accounts) == 0 {
			return fmt.Errorf("users[%d] (%s): accounts is required", i, email)
		}
		for _, acct := range u.Accounts {
			if _, ok := participating[acct]; !ok {
				return fmt.Errorf("users[%d] (%s): account %q is not in accounts", i, email, acct)
			}
		}
		if _, dup := users[email]; dup {
			return fmt.Errorf("users[%d]: duplicate email %q", i, email)
		}
		f.Users[i].Email = email
		users[email] = f.Users[i]
	}

	if tz := strings.TrimSpace(f.Settings.Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("settings.timezone: invalid %q: %w", tz, err)
		}
		f.Settings.Timezone = tz
	}
	for acct, email := range f.Settings.DefaultAssignees {
		if _, ok := participating[acct]; !ok {
			return fmt.Errorf("settings.default_assignees: account %q is not in accounts", acct)
		}
		u, ok := users[strings.ToLower(email)]
		if !ok {
			return fmt.Errorf("settings.default_assignees.%s: user %q is not declared", acct, email)
		}
		if !userHasAccount(u, acct) {
			return fmt.Errorf("settings.default_assignees.%s: %s has no access to %s", acct, email, acct)
		}
	}

	seenSlug := map[string]struct{}{}
	seenID := map[string]int{}
	for i := range f.Threads {
		t := &f.Threads[i]
		if t.Status == "" {
			t.Status = StatusNew
		}
		switch t.Status {
		case StatusNew, StatusInProgress, StatusCompleted, StatusSpam:
		default:
			return fmt.Errorf("threads[%d]: invalid status %q", i, t.Status)
		}
		if _, ok := participating[t.Account]; !ok {
			return fmt.Errorf("threads[%d]: account %q is not in accounts", i, t.Account)
		}
		if _, ok := byID[t.Account]; !ok {
			return fmt.Errorf("threads[%d]: account %q is not a configured mail account", i, t.Account)
		}
		if strings.TrimSpace(t.Customer) == "" || !strings.Contains(t.Customer, "@") {
			return fmt.Errorf("threads[%d]: customer %q is invalid", i, t.Customer)
		}
		if strings.TrimSpace(t.Subject) == "" {
			return fmt.Errorf("threads[%d]: subject is required", i)
		}
		if t.AssignedSource == "" {
			if t.AssignedTo != "" {
				t.AssignedSource = AssignManual
			}
		}
		switch t.AssignedSource {
		case "", AssignManual, AssignDefault:
		default:
			return fmt.Errorf("threads[%d]: invalid assigned_source %q", i, t.AssignedSource)
		}
		if t.AssignedTo != "" {
			u, ok := users[strings.ToLower(t.AssignedTo)]
			if !ok {
				return fmt.Errorf("threads[%d]: assigned_to %q is not a declared user", i, t.AssignedTo)
			}
			if !userHasAccount(u, t.Account) {
				return fmt.Errorf("threads[%d]: %s has no access to account %s", i, t.AssignedTo, t.Account)
			}
			t.AssignedTo = strings.ToLower(t.AssignedTo)
		}
		if t.AssignedSource == AssignManual && t.AssignedTo == "" {
			return fmt.Errorf("threads[%d]: assigned_source manual requires assigned_to", i)
		}
		if t.AssignedSource == AssignDefault && t.AssignedTo != "" {
			return fmt.Errorf("threads[%d]: assigned_source default must not set assigned_to (assignment comes from account defaults)", i)
		}
		if strings.TrimSpace(t.Draft) != "" && (t.Status == StatusCompleted || t.Status == StatusSpam) {
			return fmt.Errorf("threads[%d]: draft is cleared by a terminal status", i)
		}
		if t.ID == "" {
			t.ID = slug(t.Subject)
		}
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(t.ID) {
			return fmt.Errorf("threads[%d]: id %q must be a lowercase slug", i, t.ID)
		}
		if prev, dup := seenID[t.ID]; dup {
			return fmt.Errorf("threads[%d]: duplicate id %q (also threads[%d])", i, t.ID, prev)
		}
		seenID[t.ID] = i
		slugKey := t.Account + "/" + t.ID
		if _, dup := seenSlug[slugKey]; dup {
			return fmt.Errorf("threads[%d]: duplicate slug %q", i, slugKey)
		}
		seenSlug[slugKey] = struct{}{}

		if len(t.Messages) == 0 {
			return fmt.Errorf("threads[%d] (%s): at least one message is required", i, t.ID)
		}
		var prev time.Time
		for j := range t.Messages {
			m := &t.Messages[j]
			if m.Direction == "" {
				m.Direction = DirectionInbound
			}
			if m.Direction != DirectionInbound && m.Direction != DirectionOutbound {
				return fmt.Errorf("threads[%d].messages[%d]: invalid direction %q", i, j, m.Direction)
			}
			// A first outbound message is valid (Gina demo). Ingest attaches it after a later inbound reply.
			if m.Date.IsZero() {
				return fmt.Errorf("threads[%d].messages[%d]: date is required", i, j)
			}
			if !m.Date.After(prev) && !prev.IsZero() {
				return fmt.Errorf("threads[%d].messages[%d]: date %s is not strictly after %s", i, j, m.Date.Format(time.RFC3339), prev.Format(time.RFC3339))
			}
			prev = m.Date
			if strings.TrimSpace(m.Body) == "" && len(m.Attach) == 0 {
				return fmt.Errorf("threads[%d].messages[%d]: body or attach is required", i, j)
			}
			if m.Direction == DirectionInbound && strings.TrimSpace(m.From) == "" {
				m.From = t.Customer
			}
			if m.Direction == DirectionInbound && m.FromName == "" {
				m.FromName = t.CustomerName
			}
			for k, a := range m.Attach {
				if a.Filename == "" || a.ContentType == "" || a.File == "" {
					return fmt.Errorf("threads[%d].messages[%d].attach[%d]: filename, content_type, and file are required", i, j, k)
				}
				path := a.File
				if !filepath.IsAbs(path) {
					path = filepath.Join(f.baseDir, path)
				}
				info, err := os.Stat(path)
				if err != nil {
					return fmt.Errorf("threads[%d].messages[%d].attach[%d]: %w", i, j, k, err)
				}
				if info.IsDir() {
					return fmt.Errorf("threads[%d].messages[%d].attach[%d]: %s is a directory", i, j, k, path)
				}
			}
		}
	}
	for i, t := range f.Threads {
		if t.Reopens == "" {
			continue
		}
		target, ok := seenID[t.Reopens]
		if !ok {
			return fmt.Errorf("threads[%d]: reopens %q does not match a thread id", i, t.Reopens)
		}
		if f.Threads[target].Account != t.Account {
			return fmt.Errorf("threads[%d]: reopens %q belongs to a different account", i, t.Reopens)
		}
		if f.Threads[target].Status != StatusCompleted {
			return fmt.Errorf("threads[%d]: reopens %q must be a completed thread", i, t.Reopens)
		}
		if target >= i {
			return fmt.Errorf("threads[%d]: reopens %q must appear earlier so it is ingested first", i, t.Reopens)
		}
	}
	return nil
}

// MessageID returns the deterministic Message-ID for message n (1-based) of a thread.
func MessageID(account, threadSlug string, n int) string {
	return fmt.Sprintf("seed-%s-%s-%d@minicrm.dev", account, threadSlug, n)
}

// Slug returns the thread slug used in Message-IDs.
func (t Thread) Slug() string { return t.ID }

func userHasAccount(u User, account string) bool {
	for _, a := range u.Accounts {
		if a == account {
			return true
		}
	}
	return false
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "thread"
	}
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	return s
}
