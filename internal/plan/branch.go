package plan

import "strings"

// maxSlugBytes caps the title part of a branch name.
const maxSlugBytes = 40

// BranchName is the branch a work item's change is pushed to:
// "<short-id>_<slug>", where short-id is the first eight hex digits of the
// item's ID and slug is its title in lowercase ASCII words joined by dashes.
// A title with no ASCII letters or digits gives the short ID alone. The name
// says which item it belongs to; code finds an item's pull requests through
// its recorded outputs, not by parsing this name. Plan puts it in the
// workspace step's branch param.
func BranchName(id, title string) string {
	short := strings.ReplaceAll(id, "-", "")
	short = short[:min(8, len(short))]
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	slug := b.String()
	if len(slug) > maxSlugBytes {
		// Cut on a word edge unless the cut already falls on one.
		cut := slug[:maxSlugBytes]
		if slug[maxSlugBytes] != '-' {
			if i := strings.LastIndexByte(cut, '-'); i > 0 {
				cut = cut[:i]
			}
		}
		slug = strings.TrimSuffix(cut, "-")
	}
	if slug == "" {
		return short
	}
	return short + "_" + slug
}
