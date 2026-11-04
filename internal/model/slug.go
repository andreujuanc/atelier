package model

import "strings"

const maxSlugLen = 40

// Slugify turns a title into a slug: lowercase, runs of characters outside
// [a-z0-9] become a single "-", leading/trailing "-" trimmed, cut to 40
// characters. An empty result becomes "task".
func Slugify(title string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if sep && b.Len() > 0 {
				b.WriteByte('-')
			}
			sep = false
			b.WriteRune(r)
		} else {
			sep = true
		}
	}
	s := b.String()
	if len(s) > maxSlugLen {
		s = strings.TrimRight(s[:maxSlugLen], "-")
	}
	if s == "" {
		return "task"
	}
	return s
}
