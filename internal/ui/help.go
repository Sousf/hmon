package ui

import "strings"

// helpItem is one binding and what it does.
type helpItem struct {
	Key   string
	Label string
}

// renderHelp draws the key bindings along the bottom of a view.
//
// Keys and labels are styled apart rather than run together in one colour:
// the key is what you are scanning for, so it carries the weight, while the
// separators stay quiet enough not to compete with either.
func renderHelp(items ...helpItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		part := styleHelpKey.Render(it.Key)
		if it.Label != "" {
			part += " " + styleHelpText.Render(it.Label)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, styleHelpSep.Render(" · "))
}
