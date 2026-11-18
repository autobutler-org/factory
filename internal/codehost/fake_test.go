package codehost

import (
	"slices"
	"testing"
)

// TestFakeMergePullMarksItMerged closes the merged pull request and stamps
// when it merged, as the pull request list then shows it.
func TestFakeMergePullMarksItMerged(t *testing.T) {
	f := &Fake{}
	if _, err := f.OpenPull(t.Context(), "t", "b", "0f3c2a9e_x", "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.MergePull(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := f.FindPull(t.Context(), "0f3c2a9e_x", "closed"); err != nil || !ok || p.MergedAt == "" {
		t.Errorf("FindPull = %+v, %v, %v; want closed and merged", p, ok, err)
	}
	if _, ok, _ := f.FindPull(t.Context(), "0f3c2a9e_x", "open"); ok {
		t.Error("FindPull open found the merged pull request")
	}
	if got := f.Merged(); !slices.Equal(got, []int{1}) {
		t.Errorf("Merged = %v, want [1]", got)
	}
}

// TestFakeRetargetAndComment moves a stacked pull request onto main and
// records a comment on it.
func TestFakeRetargetAndComment(t *testing.T) {
	f := &Fake{Repo: "o/a"}
	if _, err := f.OpenPull(t.Context(), "below", "", "aaaaaaaa_below", "main"); err != nil {
		t.Fatal(err)
	}
	above, err := f.OpenPull(t.Context(), "above", "", "bbbbbbbb_above", "aaaaaaaa_below")
	if err != nil {
		t.Fatal(err)
	}
	if above.Number != 2 || above.HTMLURL != "https://github.test/o/a/pull/2" {
		t.Errorf("OpenPull = %+v, want o/a pull 2", above)
	}
	if err := f.RetargetPull(t.Context(), above.Number, "main"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := f.FindPull(t.Context(), "bbbbbbbb_above", "open"); p.Base.Ref != "main" {
		t.Errorf("base after retarget = %q, want main", p.Base.Ref)
	}
	if err := f.RetargetPull(t.Context(), 9, "main"); err == nil {
		t.Error("RetargetPull of a missing pull request: no error")
	}
	if err := f.Comment(t.Context(), 2, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := f.Comments()[2]; !slices.Equal(got, []string{"hello"}) {
		t.Errorf("Comments[2] = %q", got)
	}
}
