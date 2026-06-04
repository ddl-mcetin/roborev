// ABOUTME: Compact job metadata handling for tracking source job IDs.
// ABOUTME: Used by worker to mark source jobs as closed when compact jobs complete.

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// CompactMetadata stores source job IDs for a compact job
type CompactMetadata struct {
	SourceJobIDs []int64 `json:"source_job_ids"`
}

// ReadCompactMetadata retrieves source job IDs for a compact job
func ReadCompactMetadata(jobID int64) (*CompactMetadata, error) {
	path := compactMetadataPath(jobID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metadata file: %w", err)
	}

	var metadata CompactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("parse metadata JSON: %w", err)
	}

	return &metadata, nil
}

// DeleteCompactMetadata removes the metadata file after processing
func DeleteCompactMetadata(jobID int64) error {
	path := compactMetadataPath(jobID)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete metadata file: %w", err)
	}
	return nil
}

// compactMetadataPath returns the file path for compact job metadata
func compactMetadataPath(jobID int64) string {
	return filepath.Join(config.DataDir(), fmt.Sprintf("compact-%d.json", jobID))
}

// IsValidCompactOutput checks whether compact agent output looks like
// a real response (vs. empty or an obvious error/stack trace).
// Intentionally permissive — we don't try to parse the review content.
func IsValidCompactOutput(output string) bool {
	output = strings.TrimSpace(output)
	if output == "" {
		return false
	}

	// Reject placeholder output from agents that ran but produced no
	// review content (auth errors, empty responses, etc.).
	if output == "No review output generated" {
		return false
	}

	// Reject obvious agent error patterns at line starts
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(strings.ToLower(line))
		if strings.HasPrefix(trimmed, "error:") ||
			strings.HasPrefix(trimmed, "exception:") ||
			strings.HasPrefix(trimmed, "traceback") {
			return false
		}
	}

	return !reportsRemainingFindingsWithoutDetails(output)
}

var (
	compactFileLinePattern            = regexp.MustCompile(`(?i)\b[\w./-]+\.(go|py|js|ts|tsx|jsx|java|rb|rs|c|cc|cpp|h|hpp|cs|php|swift|kt|m|mm|sql|yaml|yml|json|toml|md):\d+\b`)
	compactZeroVerifiedFindingsPhrase = regexp.MustCompile(`\b0 verified findings\b`)
)

func reportsRemainingFindingsWithoutDetails(output string) bool {
	lower := strings.ToLower(output)
	if reportsNoRemainingFindings(lower) {
		return false
	}
	if !mentionsRemainingFindings(lower) {
		return false
	}
	return !hasActionableCompactFinding(output, lower)
}

func reportsNoRemainingFindings(lower string) bool {
	noRemainingPhrases := []string{
		"all previous findings have been addressed",
		"all findings have been resolved",
		"no issues found",
		"no verified findings remain",
		"no findings remain",
		"no remaining findings",
		"zero verified findings",
	}
	for _, phrase := range noRemainingPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return compactZeroVerifiedFindingsPhrase.MatchString(lower)
}

func mentionsRemainingFindings(lower string) bool {
	remainingPhrases := []string{
		"findings remain",
		"finding remains",
		"verified findings",
		"verified finding",
		"verdict: fail",
	}
	for _, phrase := range remainingPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func hasActionableCompactFinding(output, lower string) bool {
	if compactFileLinePattern.MatchString(output) {
		return true
	}

	return strings.Contains(lower, "**severity**:") &&
		(strings.Contains(lower, "**location**:") ||
			strings.Contains(lower, "**problem**:") ||
			strings.Contains(lower, "**fix**:"))
}
