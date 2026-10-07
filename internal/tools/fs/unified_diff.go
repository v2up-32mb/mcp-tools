package fs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/v2up-32mb/mcp-tools/internal/config"
	"github.com/v2up-32mb/mcp-tools/internal/mcp"
	"github.com/v2up-32mb/mcp-tools/internal/numconv"
)

type unifiedFilePatch struct {
	OldPath string
	NewPath string
	Hunks   []unifiedHunk
}

type unifiedHunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Section  string
	Lines    []unifiedHunkLine
}

type unifiedHunkLine struct {
	Kind byte
	Text string
}

type patchFailure struct {
	Reason          string
	HunkIndex       int
	TargetStartLine int
	ExpectedLines   []string
	ActualLines     []string
	Message         string
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: ?(.*))?$`)

func applyUnifiedDiff(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		diffText, err := requiredStringArg(args, "diff")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		if strings.TrimSpace(diffText) == "" {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("diff required"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		expectedOldText, expectedOldTextSet, err := optionalStringArg(args, "expected_old_text")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		dryRun, _, err := optionalBoolArg(args, "dry_run")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		contextLines := 2
		if v, ok, err := optionalIntArg(args, "context_lines"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			switch {
			case v < 0:
				return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("context_lines must be >= 0"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
			case v > 20:
				contextLines = 20
			default:
				contextLines = v
			}
		}

		patch, failure := parseUnifiedDiff(diffText)
		if failure != nil {
			return mcp.Result{}, wrapPatchFailure(path, failure, "invalid unified diff")
		}
		if failure = validatePatchPaths(patch, path, args["path"], cfg.StartupDirectory); failure != nil {
			return mcp.Result{}, wrapPatchFailure(path, failure, "patch path header mismatch")
		}

		payload, err := os.ReadFile(path)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "read failed"})
		}
		original := string(payload)
		if expectedOldTextSet && expectedOldText != original {
			return mcp.Result{}, wrapPatchFailure(path, &patchFailure{
				Reason:        "expected_old_text_mismatch",
				ExpectedLines: splitForStructured(expectedOldText),
				ActualLines:   splitForStructured(original),
				Message:       "expected_old_text mismatch",
			}, "expected_old_text mismatch")
		}

		updated, failure := applyPatchStrict(original, patch)
		if failure != nil {
			return mcp.Result{}, wrapPatchFailure(path, failure, "patch apply failed")
		}

		updatedBytes := []byte(updated)
		if !dryRun {
			if err := atomicWrite(path, updatedBytes); err != nil {
				return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "write failed"})
			}
		}

		affected := affectedRanges(patch.Hunks)
		updatedLines := splitLinesPreserve(updated)
		snippet := renderPatchSnippet(updatedLines, affected, contextLines)
		digest := fmt.Sprintf("%x", sha256.Sum256(updatedBytes))
		mode := "applied"
		if dryRun {
			mode = "validated"
		}
		summary := fmt.Sprintf("%s %d hunk(s)", mode, len(patch.Hunks))
		bytesWritten := 0
		if !dryRun {
			bytesWritten = len(updatedBytes)
		}

		return mcp.TextResult(summary+snippetSuffix(snippet), map[string]any{
			"summary":              summary,
			"path":                 path,
			"dry_run":              dryRun,
			"hunks_applied":        len(patch.Hunks),
			"bytes_written":        bytesWritten,
			"content_digest":       digest,
			"affected_line_ranges": affected,
			"context_snippet":      snippet,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func wrapPatchFailure(path string, failure *patchFailure, fallback string) error {
	message := fallback
	if failure != nil && strings.TrimSpace(failure.Message) != "" {
		message = failure.Message
	}
	structured := map[string]any{
		"path": path,
	}
	if failure != nil {
		structured["reason"] = failure.Reason
		if failure.HunkIndex > 0 {
			structured["hunk_index"] = failure.HunkIndex
		}
		if failure.TargetStartLine > 0 {
			structured["target_start_line"] = failure.TargetStartLine
		}
		if len(failure.ExpectedLines) > 0 {
			structured["expected_lines"] = failure.ExpectedLines
		}
		if len(failure.ActualLines) > 0 {
			structured["actual_lines"] = failure.ActualLines
		}
	}
	return mcp.WrapToolErrorWithStructured(errors.New(message), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "patch apply failed"}, structured)
}

func parseUnifiedDiff(diffText string) (unifiedFilePatch, *patchFailure) {
	lines := splitLinesPreserve(diffText)
	if len(lines) == 0 {
		return unifiedFilePatch{}, &patchFailure{Reason: "invalid_unified_diff", Message: "diff required"}
	}

	var patch unifiedFilePatch
	i := 0
	for i < len(lines) && !strings.HasPrefix(lines[i], "--- ") {
		trimmed := strings.TrimSuffix(lines[i], "\n")
		if strings.HasPrefix(trimmed, "@@ ") {
			return unifiedFilePatch{}, &patchFailure{Reason: "invalid_unified_diff", Message: "missing diff file headers"}
		}
		i++
	}
	if i >= len(lines) {
		return unifiedFilePatch{}, &patchFailure{Reason: "invalid_unified_diff", Message: "missing diff file headers"}
	}
	patch.OldPath = parseDiffHeaderPath(lines[i], "--- ")
	i++
	if i >= len(lines) || !strings.HasPrefix(lines[i], "+++ ") {
		return unifiedFilePatch{}, &patchFailure{Reason: "invalid_unified_diff", Message: "missing +++ diff header"}
	}
	patch.NewPath = parseDiffHeaderPath(lines[i], "+++ ")
	i++

	for i < len(lines) {
		trimmed := strings.TrimSuffix(lines[i], "\n")
		switch {
		case strings.HasPrefix(lines[i], "@@ "):
			hunk, next, failure := parseHunk(lines, i)
			if failure != nil {
				return unifiedFilePatch{}, failure
			}
			patch.Hunks = append(patch.Hunks, hunk)
			i = next
		case strings.HasPrefix(trimmed, "diff --git "), strings.HasPrefix(lines[i], "--- "), strings.HasPrefix(lines[i], "+++ "):
			return unifiedFilePatch{}, &patchFailure{Reason: "multi_file_diff_not_supported", Message: "multi-file diff is not supported"}
		case trimmed == "":
			i++
		default:
			// allow metadata lines like index/new file mode before the first hunk only
			i++
		}
	}
	if len(patch.Hunks) == 0 {
		return unifiedFilePatch{}, &patchFailure{Reason: "invalid_unified_diff", Message: "diff has no hunks"}
	}
	return patch, nil
}

func parseHunk(lines []string, start int) (unifiedHunk, int, *patchFailure) {
	match := hunkHeaderRE.FindStringSubmatch(strings.TrimSuffix(lines[start], "\n"))
	if match == nil {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "invalid hunk header"}
	}
	oldStart, err := parseDiffHeaderNumber(match[1])
	if err != nil {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "invalid hunk header number"}
	}
	oldCount, err := parseDiffCount(match[2])
	if err != nil {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "invalid hunk header number"}
	}
	newStart, err := parseDiffHeaderNumber(match[3])
	if err != nil {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "invalid hunk header number"}
	}
	newCount, err := parseDiffCount(match[4])
	if err != nil {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "invalid hunk header number"}
	}
	if (oldCount > 0 && oldStart == 0) || (newCount > 0 && newStart == 0) {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "hunk header non-empty range start must be positive"}
	}
	hunk := unifiedHunk{OldStart: oldStart, OldCount: oldCount, NewStart: newStart, NewCount: newCount, Section: match[5]}

	i := start + 1
	for i < len(lines) {
		trimmed := strings.TrimSuffix(lines[i], "\n")
		switch {
		case strings.HasPrefix(lines[i], "@@ "), strings.HasPrefix(trimmed, "diff --git "), strings.HasPrefix(lines[i], "--- "):
			goto validate
		case trimmed == `\ No newline at end of file`:
			if len(hunk.Lines) == 0 {
				return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "newline marker without previous diff line"}
			}
			last := &hunk.Lines[len(hunk.Lines)-1]
			last.Text = strings.TrimSuffix(last.Text, "\n")
			i++
			continue
		}
		if len(lines[i]) == 0 {
			return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "invalid empty diff line"}
		}
		kind := lines[i][0]
		if kind != ' ' && kind != '-' && kind != '+' {
			return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: fmt.Sprintf("invalid hunk line prefix %q", string(kind))}
		}
		hunk.Lines = append(hunk.Lines, unifiedHunkLine{Kind: kind, Text: lines[i][1:]})
		i++
	}

validate:
	oldSeen, newSeen := 0, 0
	for _, line := range hunk.Lines {
		switch line.Kind {
		case ' ':
			oldSeen++
			newSeen++
		case '-':
			oldSeen++
		case '+':
			newSeen++
		}
	}
	if oldSeen != hunk.OldCount || newSeen != hunk.NewCount {
		return unifiedHunk{}, 0, &patchFailure{Reason: "invalid_unified_diff", Message: "hunk line counts do not match header"}
	}
	return hunk, i, nil
}

func parseDiffHeaderNumber(raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func parseDiffCount(raw string) (int, error) {
	if raw == "" {
		return 1, nil
	}
	return parseDiffHeaderNumber(raw)
}

func parseDiffHeaderPath(line string, prefix string) string {
	rest := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "\n")
	if idx := strings.IndexByte(rest, '\t'); idx >= 0 {
		rest = rest[:idx]
	}
	return strings.TrimSpace(rest)
}

func validatePatchPaths(patch unifiedFilePatch, resolvedPath string, rawPath any, startupDir string) *patchFailure {
	candidates := make(map[string]struct{})
	addCandidate := func(value string) {
		value = canonicalDiffPath(value)
		if value != "" {
			candidates[value] = struct{}{}
		}
	}
	addCandidate(resolvedPath)
	if raw, ok := rawPath.(string); ok {
		addCandidate(raw)
	}
	if rel, err := filepath.Rel(startupDir, resolvedPath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		addCandidate(rel)
	}

	oldPath := canonicalDiffPath(patch.OldPath)
	newPath := canonicalDiffPath(patch.NewPath)
	_, oldOK := candidates[oldPath]
	_, newOK := candidates[newPath]
	if oldOK && newOK {
		return nil
	}
	expected := make([]string, 0, len(candidates))
	for value := range candidates {
		expected = append(expected, value)
	}
	sort.Strings(expected)
	return &patchFailure{
		Reason:        "path_header_mismatch",
		ExpectedLines: expected,
		ActualLines:   []string{patch.OldPath, patch.NewPath},
		Message:       "diff header paths do not match path",
	}
}

func canonicalDiffPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "/dev/null" {
		return path
	}
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		path = path[2:]
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func applyPatchStrict(original string, patch unifiedFilePatch) (string, *patchFailure) {
	base := splitLinesPreserve(original)
	result := make([]string, 0, len(base))
	prevIndex := 0

	for idx, hunk := range patch.Hunks {
		targetIndex, failure := hunkTargetIndex(hunk, len(base))
		if failure != nil {
			failure.HunkIndex = idx + 1
			return "", failure
		}
		if targetIndex < prevIndex {
			return "", &patchFailure{Reason: "header_mismatch", HunkIndex: idx + 1, TargetStartLine: hunk.OldStart, Message: "hunk overlaps or goes backwards"}
		}
		result = append(result, base[prevIndex:targetIndex]...)
		current := targetIndex
		for _, line := range hunk.Lines {
			switch line.Kind {
			case ' ':
				if current >= len(base) || base[current] != line.Text {
					return "", mismatchFailure("context_mismatch", idx+1, hunk.OldStart, line.Text, currentLine(base, current), "context line does not match target file")
				}
				result = append(result, base[current])
				current++
			case '-':
				if current >= len(base) || base[current] != line.Text {
					return "", mismatchFailure("delete_mismatch", idx+1, hunk.OldStart, line.Text, currentLine(base, current), "delete line does not match target file")
				}
				current++
			case '+':
				result = append(result, line.Text)
			}
		}
		prevIndex = current
	}

	result = append(result, base[prevIndex:]...)
	return strings.Join(result, ""), nil
}

func hunkTargetIndex(hunk unifiedHunk, totalLines int) (int, *patchFailure) {
	if hunk.OldCount == 0 {
		if hunk.OldStart < 0 || hunk.OldStart > totalLines {
			return 0, &patchFailure{Reason: "header_mismatch", TargetStartLine: hunk.OldStart, Message: "hunk header start line is out of bounds"}
		}
		return hunk.OldStart, nil
	}
	if hunk.OldStart <= 0 {
		return 0, &patchFailure{Reason: "header_mismatch", TargetStartLine: hunk.OldStart, Message: "hunk header start line must be positive"}
	}
	target := hunk.OldStart - 1
	if target > totalLines {
		return 0, &patchFailure{Reason: "header_mismatch", TargetStartLine: hunk.OldStart, Message: "hunk header start line is out of bounds"}
	}
	return target, nil
}

func mismatchFailure(reason string, hunkIndex int, targetStart int, expected string, actual string, message string) *patchFailure {
	return &patchFailure{
		Reason:          reason,
		HunkIndex:       hunkIndex,
		TargetStartLine: targetStart,
		ExpectedLines:   splitForStructured(expected),
		ActualLines:     splitForStructured(actual),
		Message:         message,
	}
}

func currentLine(lines []string, idx int) string {
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	return lines[idx]
}

func splitForStructured(text string) []string {
	if text == "" {
		return nil
	}
	parts := splitLinesPreserve(text)
	if len(parts) == 0 {
		return []string{text}
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.TrimSuffix(part, "\n"))
	}
	return out
}

func affectedRanges(hunks []unifiedHunk) []map[string]any {
	ranges := make([]map[string]any, 0, len(hunks))
	for _, hunk := range hunks {
		end := hunk.NewStart + hunk.NewCount - 1
		if hunk.NewCount == 0 {
			end = hunk.NewStart - 1
		}
		ranges = append(ranges, map[string]any{
			"old_start": hunk.OldStart,
			"old_count": hunk.OldCount,
			"new_start": hunk.NewStart,
			"new_count": hunk.NewCount,
			"new_end":   end,
		})
	}
	return ranges
}

func renderPatchSnippet(lines []string, ranges []map[string]any, contextLines int) string {
	if len(lines) == 0 || len(ranges) == 0 {
		return ""
	}
	start := 0
	end := 0
	for idx, entry := range ranges {
		rangeStart := intValue(entry["new_start"])
		rangeCount := intValue(entry["new_count"])
		if rangeCount == 0 && rangeStart > 0 {
			rangeCount = 1
		}
		rangeEnd := rangeStart + max(rangeCount, 1) - 1
		if idx == 0 || rangeStart < start {
			start = rangeStart
		}
		if idx == 0 || rangeEnd > end {
			end = rangeEnd
		}
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = start
	}
	return renderLineContext(lines, start, end, contextLines)
}

func intValue(v any) int {
	switch value := v.(type) {
	case int:
		return value
	case float64:
		parsed, ok := numconv.IntFromFloat64OK(value)
		if !ok {
			return 0
		}
		return parsed
	case json.Number:
		parsed, ok := numconv.IntFromJSONNumberOK(value)
		if !ok {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func snippetSuffix(snippet string) string {
	if strings.TrimSpace(snippet) == "" {
		return ""
	}
	return "\n" + snippet
}

func schemaApplyUnifiedDiff() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":              nonEmptyStringSchema(),
			"diff":              nonEmptyStringSchema(),
			"expected_old_text": map[string]any{"type": "string"},
			"dry_run":           map[string]any{"type": "boolean"},
			"context_lines":     map[string]any{"type": "integer", "minimum": 0},
		},
		"required": []string{"path", "diff"},
	}
}
