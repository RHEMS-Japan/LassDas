package worker

import (
	"fmt"
	"strings"
	"testing"
)

// A large file with a small edit must reach the reviewer as the changed
// region plus context, not as two full copies — locale catalogs broke the
// full-copy form on the first console ticket.
func TestChangedRegionPatchShowsOnlyTheChangedRegion(t *testing.T) {
	lines := make([]string, 0, 200)
	for index := 0; index < 200; index++ {
		lines = append(lines, strings.Repeat("x", 20))
	}
	before := strings.Join(lines, "\n")
	edited := append([]string(nil), lines...)
	edited[100] = "changed-line"
	after := strings.Join(edited, "\n")

	patch := changedRegionPatch(before, after, 3)
	if !strings.Contains(patch, "-"+strings.Repeat("x", 20)+"\n") || !strings.Contains(patch, "+changed-line\n") {
		t.Fatalf("patch lacks the edit:\n%s", patch)
	}
	if lineCount := strings.Count(patch, "\n"); lineCount > 12 {
		t.Fatalf("patch is not bounded to the region: %d lines", lineCount)
	}
	if !strings.Contains(patch, "101") {
		t.Fatalf("patch lacks the line position:\n%s", patch)
	}
}

func TestReviewFileViewsChoosesTheRightForm(t *testing.T) {
	small := "line one\nline two\n"
	smallChanged := "line one\nline 2\n"
	bigLines := make([]string, 2000)
	for index := range bigLines {
		bigLines[index] = strings.Repeat("y", 30)
	}
	big := strings.Join(bigLines, "\n")
	bigEdited := append([]string(nil), bigLines...)
	bigEdited[1500] = "edited"
	bigChanged := strings.Join(bigEdited, "\n")

	source := SourceSnapshot{Files: []SourceFile{
		{Path: "a.txt", Content: small},
		{Path: "b.txt", Content: small},
		{Path: "c.txt", Content: big},
	}}
	candidate := Candidate{Files: []CandidateFile{
		{Path: "a.txt", Content: small},
		{Path: "b.txt", Content: smallChanged},
		{Path: "c.txt", Content: bigChanged},
	}}

	views := reviewFileViews(candidate, source)
	if views[0].Status != "unchanged" || views[0].Before != "" || views[0].Patch != "" {
		t.Fatalf("unchanged view = %+v", views[0])
	}
	if views[1].Status != "replaced" || views[1].Before != small || views[1].After != smallChanged {
		t.Fatalf("small view = %+v", views[1])
	}
	if views[2].Status != "patched" || views[2].Patch == "" || views[2].Before != "" {
		t.Fatalf("large view status = %s", views[2].Status)
	}
	if len(views[2].Patch) > 2048 {
		t.Fatalf("large view patch is not bounded: %d bytes", len(views[2].Patch))
	}
	if views[2].BeforeLines != 2000 || views[2].AfterLines != 2000 {
		t.Fatalf("large view line counts = %d/%d", views[2].BeforeLines, views[2].AfterLines)
	}
}

// A change smeared across a huge file cannot fit any reviewer; the prompt
// must fail honestly instead of overflowing the model's context.
func TestReviewPromptRefusesAnOversizedView(t *testing.T) {
	bigLines := make([]string, 12000)
	for index := range bigLines {
		bigLines[index] = strings.Repeat("z", 30)
	}
	before := strings.Join(bigLines, "\n")
	edited := append([]string(nil), bigLines...)
	edited[0] = "top"
	edited[len(edited)-1] = "bottom"
	after := strings.Join(edited, "\n")

	source := SourceSnapshot{Files: []SourceFile{{Path: "big.txt", Content: before}}}
	candidate := Candidate{Files: []CandidateFile{{Path: "big.txt", Content: after}}}
	if _, err := reviewPrompt(candidate, source, TicketRequest{}, nil); err == nil {
		t.Fatal("reviewPrompt() accepted an oversized view")
	}
}

// An import added at the top and a field changed hundreds of lines below are
// two hunks, not one region spanning everything between them — the merged
// form rendered 895 lines for a two-site edit and killed its review by
// prompt size (measured on the third live marked ticket).
func TestChangedRegionPatchKeepsFarApartEditsApart(t *testing.T) {
	lines := make([]string, 0, 1000)
	for index := 0; index < 1000; index++ {
		lines = append(lines, fmt.Sprintf("line-%04d", index))
	}
	before := strings.Join(lines, "\n")
	edited := append([]string(nil), lines...)
	edited[50] = "changed-import"
	edited[950] = "changed-field"
	after := strings.Join(edited, "\n")

	patch := changedRegionPatch(before, after, 3)
	if count := strings.Count(patch, "@@ 変更前"); count != 2 {
		t.Fatalf("hunks = %d, want 2:\n%s", count, patch)
	}
	if lineCount := strings.Count(patch, "\n"); lineCount > 24 {
		t.Fatalf("patch is not bounded: %d lines", lineCount)
	}
	if !strings.Contains(patch, "-line-0050\n") || !strings.Contains(patch, "+changed-import\n") ||
		!strings.Contains(patch, "-line-0950\n") || !strings.Contains(patch, "+changed-field\n") {
		t.Fatalf("patch lacks an edit:\n%s", patch)
	}
	if !strings.Contains(patch, "@@ 変更前 51-51 行目 / 変更後 51-51 行目 @@\n") ||
		!strings.Contains(patch, "@@ 変更前 951-951 行目 / 変更後 951-951 行目 @@\n") {
		t.Fatalf("patch lacks an exact hunk header:\n%s", patch)
	}
}

// Edits closer than the context window stay in one hunk.
func TestChangedRegionPatchMergesAdjacentEdits(t *testing.T) {
	lines := make([]string, 0, 100)
	for index := 0; index < 100; index++ {
		lines = append(lines, fmt.Sprintf("line-%03d", index))
	}
	before := strings.Join(lines, "\n")
	edited := append([]string(nil), lines...)
	edited[40] = "first-edit"
	edited[44] = "second-edit"
	after := strings.Join(edited, "\n")

	patch := changedRegionPatch(before, after, 3)
	if count := strings.Count(patch, "@@ 変更前"); count != 1 {
		t.Fatalf("hunks = %d, want 1:\n%s", count, patch)
	}
}

// Edits separated by exactly twice the context stay merged; one line more
// splits them.
func TestChangedRegionPatchSplitsExactlyBeyondTwiceTheContext(t *testing.T) {
	build := func(gap int) string {
		lines := make([]string, 0, 60)
		for index := 0; index < 60; index++ {
			lines = append(lines, fmt.Sprintf("line-%02d", index))
		}
		lines[20] = "first-edit"
		lines[20+gap+1] = "second-edit"
		return strings.Join(lines, "\n")
	}
	base := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		base = append(base, fmt.Sprintf("line-%02d", index))
	}
	before := strings.Join(base, "\n")
	if patch := changedRegionPatch(before, build(6), 3); strings.Count(patch, "@@ 変更前") != 1 {
		t.Fatalf("gap of exactly 2*context must merge:\n%s", patch)
	}
	if patch := changedRegionPatch(before, build(7), 3); strings.Count(patch, "@@ 変更前") != 2 {
		t.Fatalf("gap of 2*context+1 must split:\n%s", patch)
	}
}

// A pure insertion renders only added lines with context and keeps every
// original line intact in the surrounding view.
func TestChangedRegionPatchRendersPureInsertions(t *testing.T) {
	before := "alpha\nbeta\ngamma\ndelta\nepsilon"
	after := "alpha\nbeta\ninserted-one\ninserted-two\ngamma\ndelta\nepsilon"
	patch := changedRegionPatch(before, after, 1)
	if strings.Contains(patch, "\n-") {
		t.Fatalf("pure insertion shows removals:\n%s", patch)
	}
	if !strings.Contains(patch, "+inserted-one\n+inserted-two\n") {
		t.Fatalf("insertion lines are missing or split:\n%s", patch)
	}
}

// When the changed middle is too large to align, the whole region falls back
// to one replacement — the pre-alignment behaviour.
func TestChangedRegionPatchFallsBackOnOversizedMiddles(t *testing.T) {
	buildLines := func(seed string, count int) string {
		lines := make([]string, 0, count)
		for index := 0; index < count; index++ {
			lines = append(lines, fmt.Sprintf("%s-%05d", seed, index))
		}
		return strings.Join(lines, "\n")
	}
	before := buildLines("old", 2100)
	after := buildLines("new", 2100)
	patch := changedRegionPatch(before, after, 3)
	if count := strings.Count(patch, "@@ 変更前"); count != 1 {
		t.Fatalf("fallback must render one region, got %d hunks", count)
	}
	if !strings.Contains(patch, "-old-00000\n") || !strings.Contains(patch, "+new-02099\n") {
		t.Fatalf("fallback lost content:\n%s", patch)
	}
}
