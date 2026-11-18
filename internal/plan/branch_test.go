package plan_test

import (
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/plan"
)

func TestBranchName(t *testing.T) {
	const id = "0f3c2a9e-1b2c-4d5e-8f90-123456789abc"
	for _, tc := range []struct {
		name, title, want string
	}{
		{"plain", "Add null check to the importer", "0f3c2a9e_add-null-check-to-the-importer"},
		{"punctuation collapses", "fix: crash -- when  ID is ''", "0f3c2a9e_fix-crash-when-id-is"},
		{"non-ascii is dropped", "Größe ändern", "0f3c2a9e_gr-e-ndern"},
		{"empty title", "", "0f3c2a9e"},
		{"only punctuation", "!!!", "0f3c2a9e"},
		{"long title is cut on a word edge", strings.Repeat("abcdefghi ", 10), "0f3c2a9e_abcdefghi-abcdefghi-abcdefghi-abcdefghi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := plan.BranchName(id, tc.title); got != tc.want {
				t.Errorf("BranchName(%q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}
