package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

// agentPickerEnqueueResultMsg is delivered after the daemon enqueue attempt.
// token is the per-submission identifier checked by the result handler so two
// submissions for the same jobID can't be confused (e.g. user submits, reopens
// picker on the same job, submits again, then submission #1's late result
// arrives — its token won't match the now-current submission #2's token, so
// it's correctly dropped as stale).
type agentPickerEnqueueResultMsg struct {
	token uint64
	jobID int64 // kept for human-readable flash text only; gating uses token
	agent string
	err   error
}

// handleAgentPickerOpenKey opens the agent picker modal for the currently-highlighted
// review job in the queue view. No-op if the user is not on a review job.
//
// The picker is gated to actual review jobs (single-commit, range, dirty)
// that are not panel members. Excluded job types — task/insights/compact/
// fix/classify/synthesis — need extra payload (stored prompts, panel
// metadata, etc.) the enqueue body here doesn't carry; opening for them
// would produce malformed follow-up jobs. Panel members are excluded
// because the synthesis job is the rerun handle for the whole panel,
// mirroring the existing `r` rerun behavior, and because confirm() looks
// jobs up in m.jobs which doesn't include side-fetched members.
//
// Dirty reviews are refused because diff_content is only hydrated server-side
// during ClaimJob, not on the queue listing, so the client doesn't have the
// payload required by /api/enqueue for git_ref="dirty". Letting the picker
// open and enqueue would silently produce a broken job.
func (m model) handleAgentPickerOpenKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m, nil
	}
	job, ok := m.selectedJob()
	if !ok {
		return m, nil
	}
	// Opening the picker (even on the same job again) counts as moving on:
	// any still-pending enqueue result becomes stale and must not surface
	// on top of this modal. Clearing the token rather than jobID also
	// covers the submit-twice-on-same-job case, where the jobID would
	// otherwise still match.
	m.pendingEnqueueToken = 0
	if reason, eligible := agentPickerEligibility(job); !eligible {
		m.agentPickerErr = reason
		m.currentView = viewAgentPicker
		m.agentPickerJobID = job.ID
		m.agentPickerJobInfo = formatAgentPickerHeader(job)
		m.agentPickerAgents = nil
		m.agentPickerIdx = 0
		return m, nil
	}

	m.agentPickerAgents = agent.KnownAgentNames()
	m.agentPickerJobID = job.ID
	m.agentPickerJobInfo = formatAgentPickerHeader(job)
	m.agentPickerIdx = indexOfAgent(job.Agent, m.agentPickerAgents)
	m.agentPickerErr = ""
	m.currentView = viewAgentPicker
	return m, nil
}

// agentPickerEligibility reports whether this job can be re-run via the
// agent picker. The reason string is suitable for display to the user.
func agentPickerEligibility(job *storage.ReviewJob) (reason string, eligible bool) {
	if job.PanelRole == storage.PanelRoleMember {
		return "select the panel's synthesis row to re-run the panel", false
	}
	if !job.IsReviewJob() {
		return "agent picker only supports commit / range reviews", false
	}
	if isDirtyJob(job) {
		return "agent picker doesn't support dirty reviews yet", false
	}
	return "", true
}

// isDirtyJob reports whether the job represents an uncommitted-changes review.
// Delegates to ReviewJob.IsDirtyJob() so JobTypeDirty is honored as the
// authoritative signal — that catches dirty rows whose DiffContent isn't
// hydrated by the queue listing and whose git_ref happens to be something
// other than the literal "dirty" string.
func isDirtyJob(job *storage.ReviewJob) bool {
	return job.IsDirtyJob()
}

// handleAgentPickerKey routes keys while the agent picker modal is open.
func (m model) handleAgentPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.currentView = viewQueue
		m.agentPickerErr = ""
		// Cancelling counts as moving on — drop any in-flight submission.
		m.pendingEnqueueToken = 0
		return m, nil
	case "up", "k":
		if m.agentPickerIdx > 0 {
			m.agentPickerIdx--
		}
		return m, nil
	case "down", "j":
		if m.agentPickerIdx < len(m.agentPickerAgents)-1 {
			m.agentPickerIdx++
		}
		return m, nil
	case "home", "g":
		m.agentPickerIdx = 0
		return m, nil
	case "end", "G":
		m.agentPickerIdx = len(m.agentPickerAgents) - 1
		return m, nil
	case "enter":
		return m.confirmAgentPicker()
	}
	return m, nil
}

// confirmAgentPicker enqueues a new review job for the selected agent and closes the modal.
func (m model) confirmAgentPicker() (tea.Model, tea.Cmd) {
	if m.agentPickerIdx < 0 || m.agentPickerIdx >= len(m.agentPickerAgents) {
		return m, nil
	}
	chosen := m.agentPickerAgents[m.agentPickerIdx]
	// Re-resolve via selectedJob() so we hit the same scope used at open
	// time (m.jobs + side-fetched panel members), then re-check eligibility
	// in case the job's state changed between A and Enter (e.g. a queue
	// refresh promoted it into a panel or marked it dirty).
	job, ok := m.selectedJob()
	if !ok || job.ID != m.agentPickerJobID {
		m.agentPickerErr = "job no longer in queue; close and retry"
		return m, nil
	}
	if reason, eligible := agentPickerEligibility(job); !eligible {
		m.agentPickerErr = reason
		return m, nil
	}
	// Close immediately; the async result message will surface success
	// (queue refresh) or failure (flash) via handleAgentPickerEnqueueResultMsg.
	jobID := m.agentPickerJobID
	jobSnapshot := *job
	m.currentView = viewQueue
	m.agentPickerErr = ""
	m.nextEnqueueToken++
	token := m.nextEnqueueToken
	m.pendingEnqueueToken = token

	return m, m.enqueueWithAgent(token, jobID, chosen, jobSnapshot)
}

// agentPickerEnqueueBody mirrors a subset of daemon.EnqueueRequest. The
// generated client is missing `panel` and `strict_agent`, so we issue a raw
// POST (same pattern as fetchPanelMembers / fetchJobLog). Dirty reviews are
// refused at the picker open step, so diff_content is intentionally omitted
// here.
type agentPickerEnqueueBody struct {
	RepoPath     string `json:"repo_path"`
	GitRef       string `json:"git_ref,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Agent        string `json:"agent,omitempty"`
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Reasoning    string `json:"reasoning,omitempty"`
	ReviewType   string `json:"review_type,omitempty"`
	JobType      string `json:"job_type,omitempty"`
	Agentic      bool   `json:"agentic,omitempty"`
	MinSeverity  string `json:"min_severity,omitempty"`
	OutputPrefix string `json:"output_prefix,omitempty"`
	Panel        string `json:"panel,omitempty"`
	StrictAgent  bool   `json:"strict_agent,omitempty"`
}

// enqueueWithAgent calls the daemon to create a new review job for the same commit
// using the chosen agent. The original job is left as-is — users keep both records.
//
// All review-context fields (review_type, agentic, min_severity, reasoning,
// model/provider, diff_content for dirty reviews, etc.) are copied from the
// original job so the rerun matches the original's intent. panel="none" is
// forced so the choice can't get expanded into a panel run in repos that
// configure a default panel.
func (m model) enqueueWithAgent(token uint64, originalJobID int64, agentName string, job storage.ReviewJob) tea.Cmd {
	return func() tea.Msg {
		body := buildAgentPickerEnqueueBody(job, agentName)
		err := postEnqueueRaw(m, body)
		return agentPickerEnqueueResultMsg{
			token: token,
			jobID: originalJobID,
			agent: agentName,
			err:   err,
		}
	}
}

// buildAgentPickerEnqueueBody is split out from enqueueWithAgent so the
// payload shape can be unit-tested directly. Notes on field choices:
//
//   - Only forward an explicit model/provider request when the original
//     job had one. The daemon treats these fields as user-requested
//     overrides; sending the original agent's *resolved* model/provider
//     would pin the new job to the wrong agent's defaults. Leaving them
//     empty lets /api/enqueue resolve the chosen agent's normal model.
//
//   - Prefer WorktreePath over RepoPath: jobs created from a linked
//     worktree have RepoPath set to the main repo root and the checkout
//     path hydrated separately in WorktreePath. Passing the worktree
//     path lets the daemon detect the worktree (via gitrepo.Root vs
//     MainRoot) and run the new review against the same checkout.
//
//   - Panel: "none" forces a single-agent run even in repos that
//     configure a default panel.
//
//   - StrictAgent: true tells the daemon to fail rather than silently
//     fall back when the requested agent isn't installed or is
//     overridden by workflow config. The user explicitly picked an
//     agent — receiving a different one without notice would defeat
//     the whole point of the picker.
//
//   - OutputPrefix carries panel-member prefixing and other reviewer-
//     attached headers. ListJobs hydrates it so queue selections forward
//     the prefix on rerun.
func buildAgentPickerEnqueueBody(job storage.ReviewJob, agentName string) agentPickerEnqueueBody {
	repoPath := job.RepoPath
	if job.WorktreePath != "" {
		repoPath = job.WorktreePath
	}
	return agentPickerEnqueueBody{
		RepoPath:     repoPath,
		GitRef:       job.GitRef,
		Branch:       job.Branch,
		Agent:        agentName,
		Model:        job.RequestedModel,
		Provider:     job.RequestedProvider,
		Reasoning:    job.Reasoning,
		ReviewType:   job.ReviewType,
		JobType:      job.JobType,
		Agentic:      job.Agentic,
		MinSeverity:  job.MinSeverity,
		OutputPrefix: job.OutputPrefix,
		Panel:        "none",
		StrictAgent:  true,
	}
}

// agentPickerSkippedError represents /api/enqueue returning 200 OK with
// {skipped: true, reason: "..."} — e.g. when the branch or commit message is
// configured to be excluded from reviews. Distinguishing this from 201
// Created lets the picker surface the reason instead of silently closing
// after a no-op enqueue.
type agentPickerSkippedError struct {
	Reason string
}

func (e *agentPickerSkippedError) Error() string {
	if e.Reason == "" {
		return "enqueue skipped"
	}
	return "enqueue skipped: " + e.Reason
}

// postEnqueueRaw issues a raw POST to /api/enqueue so we can send fields
// (panel, source) that the generated client doesn't expose. Only 201
// Created is treated as a real enqueue. A 200 OK is interpreted as a
// skipped enqueue (excluded branch/commit) and returned as a typed error
// so the result handler can show the reason.
func postEnqueueRaw(m model, body agentPickerEnqueueBody) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := m.endpoint.BaseURL() + "/api/enqueue"
	req, err := http.NewRequestWithContext(m.apiContext(), http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusOK:
		var skip struct {
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}
		if jerr := json.Unmarshal(respBody, &skip); jerr == nil && skip.Skipped {
			return &agentPickerSkippedError{Reason: skip.Reason}
		}
		// 200 without a skipped flag is unexpected for this endpoint; treat
		// as an error rather than silently succeeding.
		return apiStatusError(resp.StatusCode, resp.Status, respBody)
	default:
		return apiStatusError(resp.StatusCode, resp.Status, respBody)
	}
}

// formatAgentPickerHeader builds a short label shown in the modal header.
func formatAgentPickerHeader(job *storage.ReviewJob) string {
	sha := job.GitRef
	if len(sha) > 12 {
		sha = sha[:12]
	}
	repo := job.RepoPath
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	return fmt.Sprintf("%s @ %s — currently %s", repo, sha, job.Agent)
}

// indexOfAgent returns the index of 'name' in agents, or 0 if not found.
func indexOfAgent(name string, agents []string) int {
	for i, a := range agents {
		if a == name {
			return i
		}
	}
	return 0
}

// handleAgentPickerEnqueueResultMsg surfaces enqueue results without
// interrupting whatever the user has moved on to. Enqueue is async so a
// slow failure for job A must not pop the picker after the user has opened
// it for job B, cancelled, or just navigated away.
//
// Gating is by per-submission token (not jobID): submitting the same job
// twice in a row creates two distinct tokens, so submission #1's late
// result can't masquerade as #2's, clear the pending token, or cause #2's
// real result to be dropped as stale.
//
// Rules:
//   - msg.token != pendingEnqueueToken — stale. Drop the message.
//     Stale successes still refresh the queue so the new job is visible.
//   - msg.token matches and error — surface via setWarningFlash on the
//     current view rather than reopening the modal.
//   - msg.token matches and success — refresh the queue.
//   - Either way: clear pendingEnqueueToken so a duplicate result message
//     can't be re-processed.
func (m model) handleAgentPickerEnqueueResultMsg(msg agentPickerEnqueueResultMsg) (tea.Model, tea.Cmd) {
	if msg.token == 0 || msg.token != m.pendingEnqueueToken {
		if msg.err == nil {
			return m, m.fetchJobs()
		}
		return m, nil
	}
	m.pendingEnqueueToken = 0
	if msg.err != nil {
		var skip *agentPickerSkippedError
		var flashMsg string
		if errors.As(msg.err, &skip) {
			flashMsg = fmt.Sprintf("Skipped enqueue (%s): %s", msg.agent, skip.Reason)
		} else {
			flashMsg = fmt.Sprintf("Enqueue with %s failed: %v", msg.agent, msg.err)
		}
		m.setWarningFlash(flashMsg, agentPickerFlashDuration, m.currentView)
		return m, nil
	}
	return m, m.fetchJobs()
}

// agentPickerFlashDuration is long enough to read a multi-word error
// without being obtrusive. Kept as a constant so tests can reference it.
const agentPickerFlashDuration = 5 * time.Second
