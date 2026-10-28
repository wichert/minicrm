package mail

import (
	"encoding/json"
	"errors"
	"html"
	"strings"
)

// replyDocument is a deliberately small ProseMirror document. Raw HTML and
// unrecognized nodes or marks are never interpreted as markup.
type replyDocument struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content []replyDocument `json:"content"`
	Marks   []struct {
		Type string `json:"type"`
	} `json:"marks"`
}

// FormatReplyDocument validates the editor document and returns both alternatives.
func FormatReplyDocument(data string) (string, string, error) {
	if len(data) > 1<<20 {
		return "", "", errors.New("formatted reply is too large")
	}
	var doc replyDocument
	if err := json.Unmarshal([]byte(data), &doc); err != nil || doc.Type != "doc" || len(doc.Content) > 10000 {
		return "", "", errors.New("invalid formatted reply")
	}
	var text, markup strings.Builder
	for i, p := range doc.Content {
		if p.Type != "paragraph" || len(p.Content) > 10000 {
			return "", "", errors.New("invalid reply paragraph")
		}
		if i > 0 {
			text.WriteByte('\n')
		}
		markup.WriteString("<p>")
		if len(p.Content) == 0 {
			markup.WriteString("<br>")
		}
		for _, node := range p.Content {
			switch node.Type {
			case "hard_break":
				text.WriteByte('\n')
				markup.WriteString("<br>")
			case "text":
				text.WriteString(node.Text)
				fragment := html.EscapeString(node.Text)
				bold, italic := false, false
				for _, mark := range node.Marks {
					switch mark.Type {
					case "strong":
						bold = true
					case "em":
						italic = true
					default:
						return "", "", errors.New("invalid reply formatting")
					}
				}
				if italic {
					fragment = "<i>" + fragment + "</i>"
				}
				if bold {
					fragment = "<b>" + fragment + "</b>"
				}
				markup.WriteString(fragment)
			default:
				return "", "", errors.New("invalid reply content")
			}
		}
		markup.WriteString("</p>")
	}
	return text.String(), markup.String(), nil
}
