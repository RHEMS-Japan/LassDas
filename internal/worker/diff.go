package worker

import (
	"bytes"
	"fmt"
	"strings"
)

// The chat reviewer used to receive every target file twice — the full
// original and the full candidate — which caps the reviewable file size at a
// fraction of the model's context. Locale catalogs and generated-style files
// broke that cap on the first console ticket (250KB of JSON for a four-line
// wording change). Large changed files are therefore handed over as a
// bounded, deterministic patch: the shared head and tail are trimmed, the
// changed middle is shown as removed and added lines with a little context,
// and small files keep the exact full before/after view.

const (
	// reviewEmbedWholeFileBytes is the per-file size under which the review
	// prompt shows complete before/after contents instead of a patch.
	reviewEmbedWholeFileBytes = 24 * 1024
	reviewDiffContextLines    = 3
	// MaxReviewPromptBytes bounds the chat review prompt. A change whose
	// patch view still exceeds it fails honestly instead of overflowing the
	// reviewer's context mid-flight.
	MaxReviewPromptBytes = 320 * 1024
)

// maxDiffAlignmentCells bounds the line-alignment table. Beyond it the change
// is a near-rewrite; the single-region view is the honest rendering and the
// prompt size guard judges whether it still fits.
const maxDiffAlignmentCells = 4_000_000

// changedRegionPatch renders the edits between two file versions as hunks:
// each run of changed lines with shared context, far-apart edits kept apart.
// The first single-region form merged everything between the first and last
// edit, and an ordinary two-site edit (an import plus a form field 900 lines
// below) rendered 895 lines and killed its review by prompt size. Alignment
// is a longest-common-subsequence table over the trimmed middle, chosen for
// being easy to verify; when the middle is too large to align (a huge
// rewrite, or edits at both ends of a huge file that defeat the trim), the
// whole region is shown as one replacement exactly as before and the prompt
// size guard judges it.
func changedRegionPatch(before, after string, contextLines int) string {
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	prefix := 0
	for prefix < len(beforeLines) && prefix < len(afterLines) && beforeLines[prefix] == afterLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(beforeLines)-prefix && suffix < len(afterLines)-prefix &&
		beforeLines[len(beforeLines)-1-suffix] == afterLines[len(afterLines)-1-suffix] {
		suffix++
	}
	removed := beforeLines[prefix : len(beforeLines)-suffix]
	added := afterLines[prefix : len(afterLines)-suffix]

	ops, aligned := alignLines(removed, added)
	if !aligned {
		ops = append(bytes.Repeat([]byte{'-'}, len(removed)), bytes.Repeat([]byte{'+'}, len(added))...)
	}
	return renderHunks(beforeLines, afterLines, prefix, removed, added, ops, contextLines)
}

// alignLines returns one op per line pair step: '=' keeps a line, '-' removes
// from before, '+' adds from after. Deletions are emitted before insertions
// at the same position.
func alignLines(removed, added []string) ([]byte, bool) {
	if len(removed) == 0 || len(added) == 0 {
		return append(bytes.Repeat([]byte{'-'}, len(removed)), bytes.Repeat([]byte{'+'}, len(added))...), true
	}
	if len(removed) > maxDiffAlignmentCells/len(added) {
		return nil, false
	}
	width := len(added) + 1
	table := make([]uint16, (len(removed)+1)*width)
	for i := len(removed) - 1; i >= 0; i-- {
		for j := len(added) - 1; j >= 0; j-- {
			if removed[i] == added[j] {
				table[i*width+j] = table[(i+1)*width+j+1] + 1
			} else {
				table[i*width+j] = max(table[(i+1)*width+j], table[i*width+j+1])
			}
		}
	}
	ops := make([]byte, 0, len(removed)+len(added))
	i, j := 0, 0
	for i < len(removed) && j < len(added) {
		switch {
		case removed[i] == added[j]:
			ops = append(ops, '=')
			i++
			j++
		case table[(i+1)*width+j] >= table[i*width+j+1]:
			ops = append(ops, '-')
			i++
		default:
			ops = append(ops, '+')
			j++
		}
	}
	ops = append(ops, bytes.Repeat([]byte{'-'}, len(removed)-i)...)
	ops = append(ops, bytes.Repeat([]byte{'+'}, len(added)-j)...)
	return ops, true
}

// renderHunks walks the ops, groups changed runs whose gaps exceed twice the
// context into separate hunks, and prints each hunk in the established form:
// a header naming the changed spans, context lines around them, then the
// removed and added lines.
func renderHunks(beforeLines, afterLines []string, prefix int, removed, added []string, ops []byte, contextLines int) string {
	type edit struct {
		op         byte
		beforeLine int // index into beforeLines for '-' and '='
		afterLine  int // index into afterLines for '+' and '='
	}
	edits := make([]edit, 0, len(ops))
	i, j := 0, 0
	for _, op := range ops {
		entry := edit{op: op, beforeLine: prefix + i, afterLine: prefix + j}
		switch op {
		case '=':
			i++
			j++
		case '-':
			i++
		case '+':
			j++
		}
		edits = append(edits, entry)
	}

	var builder strings.Builder
	position := 0
	for position < len(edits) {
		if edits[position].op == '=' {
			position++
			continue
		}
		start := position
		end := position
		gap := 0
		for cursor := position + 1; cursor < len(edits); cursor++ {
			if edits[cursor].op == '=' {
				gap++
				if gap > contextLines*2 {
					break
				}
				continue
			}
			end = cursor
			gap = 0
		}

		first := edits[start]
		last := edits[end]
		beforeStart, beforeEnd := first.beforeLine, last.beforeLine
		if last.op == '-' {
			beforeEnd++
		}
		afterStart, afterEnd := first.afterLine, last.afterLine
		if last.op == '+' {
			afterEnd++
		}
		fmt.Fprintf(&builder, "@@ 変更前 %d-%d 行目 / 変更後 %d-%d 行目 @@\n",
			beforeStart+1, beforeEnd, afterStart+1, afterEnd)

		contextStart := first.beforeLine - contextLines
		if contextStart < 0 {
			contextStart = 0
		}
		for _, line := range beforeLines[contextStart:first.beforeLine] {
			builder.WriteString(" " + line + "\n")
		}
		for _, entry := range edits[start : end+1] {
			switch entry.op {
			case '=':
				builder.WriteString(" " + beforeLines[entry.beforeLine] + "\n")
			case '-':
				builder.WriteString("-" + beforeLines[entry.beforeLine] + "\n")
			case '+':
				builder.WriteString("+" + afterLines[entry.afterLine] + "\n")
			}
		}
		tailStart := beforeEnd
		tailEnd := tailStart + contextLines
		if tailEnd > len(beforeLines) {
			tailEnd = len(beforeLines)
		}
		for _, line := range beforeLines[tailStart:tailEnd] {
			builder.WriteString(" " + line + "\n")
		}
		position = end + 1
	}
	return builder.String()
}

// reviewFileView is one target file as the chat reviewer sees it.
type reviewFileView struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// Full contents for small changed files; exact and lossless.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Patch for large changed files: shared head and tail trimmed away.
	Patch       string `json:"patch,omitempty"`
	BeforeLines int    `json:"before_lines,omitempty"`
	AfterLines  int    `json:"after_lines,omitempty"`
}

func reviewFileViews(candidate Candidate, source SourceSnapshot) []reviewFileView {
	views := make([]reviewFileView, 0, len(candidate.Files))
	for index, file := range candidate.Files {
		base := source.Files[index]
		if file.Content == base.Content {
			views = append(views, reviewFileView{Path: file.Path, Status: "unchanged"})
			continue
		}
		if base.Created {
			// The reviewer judges a new file as a whole, told plainly that
			// nothing existed before it.
			views = append(views, reviewFileView{Path: file.Path, Status: "created", After: file.Content})
			continue
		}
		if len(base.Content) <= reviewEmbedWholeFileBytes && len(file.Content) <= reviewEmbedWholeFileBytes {
			views = append(views, reviewFileView{
				Path: file.Path, Status: "replaced", Before: base.Content, After: file.Content,
			})
			continue
		}
		views = append(views, reviewFileView{
			Path: file.Path, Status: "patched",
			Patch:       changedRegionPatch(base.Content, file.Content, reviewDiffContextLines),
			BeforeLines: 1 + strings.Count(base.Content, "\n"),
			AfterLines:  1 + strings.Count(file.Content, "\n"),
		})
	}
	return views
}

// ChangedRegionSummaries renders every changed file as its bounded patch,
// for the reviewing agent's instruction. Handing the reviewer the change up
// front lets it review catalog-sized files without reading them whole — a
// full read of two 200KB locale catalogs is what disconnected the first
// console review before any verdict.
func ChangedRegionSummaries(candidate Candidate, source SourceSnapshot) []string {
	summaries := make([]string, 0, len(candidate.Files))
	for index, file := range candidate.Files {
		if index >= len(source.Files) {
			continue
		}
		base := source.Files[index]
		// A created file is judged as a whole, said plainly: the generic
		// diff of "" against the content rendered it as an existing one-line
		// file with an inverted 1-0 range. This branch also keeps an empty
		// created file visible - it equals its (empty) base and the
		// unchanged-skip below would drop its section entirely.
		if base.Created {
			summaries = append(summaries, createdFileHeader(file),
				createdFileSpan(file)+"\n+"+strings.ReplaceAll(file.Content, "\n", "\n+"))
			continue
		}
		if file.Content == base.Content {
			continue
		}
		header := fmt.Sprintf("#### %s (変更前 %d 行 / 変更後 %d 行)",
			file.Path, 1+strings.Count(base.Content, "\n"), 1+strings.Count(file.Content, "\n"))
		summaries = append(summaries, header, changedRegionPatch(base.Content, file.Content, reviewDiffContextLines))
	}
	return summaries
}

func createdFileHeader(file CandidateFile) string {
	return fmt.Sprintf("#### %s (新規作成・全 %d 行)", file.Path, 1+strings.Count(file.Content, "\n"))
}

func createdFileSpan(file CandidateFile) string {
	return fmt.Sprintf("@@ 新規作成 1-%d 行目 @@", 1+strings.Count(file.Content, "\n"))
}

// ChangedRegionOutlines lists where each file changed — the hunk headers of
// the same patches, without their content. It exists for candidates whose
// full patches do not fit in an instruction: the reviewer still learns which
// line ranges to open instead of rereading whole files, which is what
// disconnected the first console review.
func ChangedRegionOutlines(candidate Candidate, source SourceSnapshot) []string {
	outlines := make([]string, 0, len(candidate.Files))
	for index, file := range candidate.Files {
		if index >= len(source.Files) {
			continue
		}
		base := source.Files[index]
		if base.Created {
			outlines = append(outlines, createdFileHeader(file), createdFileSpan(file))
			continue
		}
		if file.Content == base.Content {
			continue
		}
		header := fmt.Sprintf("#### %s (変更前 %d 行 / 変更後 %d 行)",
			file.Path, 1+strings.Count(base.Content, "\n"), 1+strings.Count(file.Content, "\n"))
		spans := make([]string, 0, 8)
		for _, line := range strings.Split(changedRegionPatch(base.Content, file.Content, reviewDiffContextLines), "\n") {
			// Content lines always carry a ' ', '-' or '+' prefix; only
			// hunk headers begin with "@@".
			if strings.HasPrefix(line, "@@") {
				spans = append(spans, line)
			}
		}
		if len(spans) == 0 {
			spans = append(spans, "(変更位置を特定できませんでした — このファイルは全体を確認)")
		}
		outlines = append(outlines, header, strings.Join(spans, "\n"))
	}
	return outlines
}
