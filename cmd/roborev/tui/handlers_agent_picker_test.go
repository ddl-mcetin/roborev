package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

// makeReviewJob constructs a job that passes ReviewJob.IsReviewJob() so the
// agent picker accepts it. Default makeJob() leaves JobType empty and has no
// CommitID/range/dirty signals, which means the legacy heuristic in
// IsReviewJob() classifies it as non-review.
func makeReviewJob(id int64, opts ...func(*storage.ReviewJob)) storage.ReviewJob {
	j := makeJob(id, opts...)
	if j.JobType == "" {
		j.JobType = storage.JobTypeReview
	}
	return j
}

func TestAgentPicker_OpenSetsViewAndDefaultsToCurrentAgent(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeReviewJob(42,
			withAgent("gemini"),
			withRepoPath("/repos/foo"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 42

	result, _ := pressKey(m, 'A')

	assert.Equal(t, viewAgentPicker, result.currentView, "A should open the agent picker")
	assert.Equal(t, int64(42), result.agentPickerJobID, "picker should remember the selected job ID")
	assert.NotEmpty(t, result.agentPickerAgents, "agent list should be populated on open")
	// Default cursor lands on the currently-used agent.
	assert.Equal(t, "gemini", result.agentPickerAgents[result.agentPickerIdx],
		"cursor should default to the current agent")
}

func TestAgentPicker_OpenSourceMatchesAgentPackage(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(1, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 1

	result, _ := pressKey(m, 'A')

	assert.Equal(t, agent.KnownAgentNames(), result.agentPickerAgents,
		"picker should source agent list from agent.KnownAgentNames")
}

func TestAgentPicker_OpenIsNoOpWhenNotOnQueue(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewReview // anything other than queue
	m.jobs = []storage.ReviewJob{makeReviewJob(1)}
	m.selectedIdx = 0
	m.selectedJobID = 1

	result, _ := m.handleAgentPickerOpenKey()

	assert.Equal(t, viewReview, result.(model).currentView, "picker should not open outside queue view")
}

func TestAgentPicker_NavigationUpDown(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(1, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := pressKey(m, 'A')
	startIdx := m2.agentPickerIdx

	m3, _ := pressKey(m2, 'j')
	assert.Equal(t, startIdx+1, m3.agentPickerIdx, "j moves down one")

	m4, _ := pressKey(m3, 'k')
	assert.Equal(t, startIdx, m4.agentPickerIdx, "k moves up one")
}

func TestAgentPicker_NavigationBounds(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(1, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := pressKey(m, 'A')
	last := len(m2.agentPickerAgents) - 1

	// k at index 0 stays at 0
	m3, _ := pressKey(m2, 'k')
	assert.Equal(t, m2.agentPickerIdx, m3.agentPickerIdx, "k at top is a no-op")

	// G goes to end
	m4, _ := pressKey(m3, 'G')
	assert.Equal(t, last, m4.agentPickerIdx, "G jumps to last")

	// j at last stays
	m5, _ := pressKey(m4, 'j')
	assert.Equal(t, last, m5.agentPickerIdx, "j at bottom is a no-op")
}

func TestAgentPicker_EscClosesWithoutEnqueue(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(1, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := pressKey(m, 'A')
	assert.Equal(t, viewAgentPicker, m2.currentView)

	m3, cmd := pressSpecial(m2, tea.KeyEsc)
	assert.Equal(t, viewQueue, m3.currentView, "esc should return to queue")
	assert.Nil(t, cmd, "esc should not produce an enqueue command")
}

func TestAgentPicker_EnterReturnsEnqueueCommand(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeReviewJob(99,
			withAgent("codex"),
			withRepoPath("/repos/example/project"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 99

	m2, _ := pressKey(m, 'A')
	// Move to a different agent.
	m3, _ := pressKey(m2, 'j')
	assert.NotEqual(t, m2.agentPickerIdx, m3.agentPickerIdx, "cursor should have moved")

	m4, cmd := pressSpecial(m3, tea.KeyEnter)
	assert.Equal(t, viewQueue, m4.currentView, "enter should close the picker")
	assert.NotNil(t, cmd, "enter on a valid selection should produce an enqueue command")
}

func TestAgentPicker_EnqueueErrorFlashesNotReopens(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.pendingEnqueueToken = 42

	msg := agentPickerEnqueueResultMsg{
		token: 42,
		jobID: 7,
		agent: "gemini",
		err:   assert.AnError,
	}
	result, _ := m.handleAgentPickerEnqueueResultMsg(msg)
	updated := result.(model)

	assert.Equal(t, viewQueue, updated.currentView,
		"async errors must NOT reopen the modal — interrupting whatever the user moved on to is the bug being fixed")
	assert.Contains(t, updated.flashMessage, "gemini",
		"the error should surface as a flash containing the agent name")
	assert.True(t, updated.flashWarning, "must be styled as a warning")
	assert.Equal(t, uint64(0), updated.pendingEnqueueToken,
		"the pending token should be cleared once processed")
}

func TestAgentPicker_SkippedResultFlashesWithReason(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.pendingEnqueueToken = 42

	msg := agentPickerEnqueueResultMsg{
		token: 42,
		jobID: 7,
		agent: "gemini",
		err:   &agentPickerSkippedError{Reason: `branch "main" is excluded from reviews`},
	}
	result, _ := m.handleAgentPickerEnqueueResultMsg(msg)
	updated := result.(model)

	assert.Equal(t, viewQueue, updated.currentView,
		"skipped enqueues flash; they do not reopen the modal")
	assert.Contains(t, updated.flashMessage, "Skipped",
		"flash should explain it was skipped")
	assert.Contains(t, updated.flashMessage, "excluded from reviews",
		"daemon's reason must be in the flash")
}

func TestAgentPicker_StaleErrorResultIsDroppedSilently(t *testing.T) {
	// User submitted A (or just closed the picker), then moved on. A's
	// delayed failure must not pop a flash or modal.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.pendingEnqueueToken = 0 // user has moved on; no submission in flight

	msg := agentPickerEnqueueResultMsg{
		token: 17, // some prior submission's token
		jobID: 100,
		agent: "gemini",
		err:   assert.AnError,
	}
	result, cmd := m.handleAgentPickerEnqueueResultMsg(msg)
	updated := result.(model)

	assert.Empty(t, updated.flashMessage,
		"stale failure must not surface any flash")
	assert.Empty(t, updated.agentPickerErr,
		"stale failure must not overwrite picker state")
	assert.Nil(t, cmd, "no command should fire for a dropped stale failure")
}

func TestAgentPicker_StaleSuccessStillRefreshesQueue(t *testing.T) {
	// A late-arriving success shouldn't surface a flash, but the queue
	// should still refresh so the new job appears.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.pendingEnqueueToken = 0

	msg := agentPickerEnqueueResultMsg{
		token: 17,
		jobID: 100,
		agent: "gemini",
		err:   nil,
	}
	result, cmd := m.handleAgentPickerEnqueueResultMsg(msg)
	updated := result.(model)

	assert.NotNil(t, cmd, "stale success should still refresh the queue")
	assert.Empty(t, updated.flashMessage,
		"stale success should not surface any flash")
}

func TestAgentPicker_OpeningPickerClearsPendingSubmission(t *testing.T) {
	// If the user re-opens the picker (same or different job) before the
	// previous submission's result arrives, that previous submission must
	// become stale so its result can't pop a flash on top of the new modal.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(50, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 50
	m.pendingEnqueueToken = 99 // some earlier submission still in flight

	result, _ := pressKey(m, 'A')

	assert.Equal(t, uint64(0), result.pendingEnqueueToken,
		"opening the picker must clear any earlier pending submission")
}

func TestAgentPicker_EscClearsPendingSubmission(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(50, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 50

	m2, _ := pressKey(m, 'A')
	require.Equal(t, viewAgentPicker, m2.currentView)
	// Simulate a prior submission still pending alongside the open modal —
	// shouldn't happen via the UI but guards the invariant defensively.
	m2.pendingEnqueueToken = 77

	m3, _ := pressSpecial(m2, tea.KeyEsc)
	assert.Equal(t, uint64(0), m3.pendingEnqueueToken,
		"esc clears the pending submission so an in-flight failure can't flash later")
}

func TestAgentPicker_TwoSubmissionsSameJob_FirstResultDoesNotMasqueradeAsSecond(t *testing.T) {
	// Submit A → token 1 pending. Reopen picker on the same A, submit
	// again → token 2 pending (1 is now stale). When submission #1's
	// failure arrives, it must be dropped (not flashed) and must NOT
	// clear pendingEnqueueToken — otherwise submission #2's real result
	// would be dropped as stale when it arrives.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.pendingEnqueueToken = 2 // submission #2 is the in-flight one

	// Late-arriving result for submission #1 (token 1, same jobID).
	stale := agentPickerEnqueueResultMsg{
		token: 1,
		jobID: 50,
		agent: "gemini",
		err:   assert.AnError,
	}
	mid, _ := m.handleAgentPickerEnqueueResultMsg(stale)
	midModel := mid.(model)

	assert.Equal(t, uint64(2), midModel.pendingEnqueueToken,
		"stale submission #1 result must not clear the pending token for submission #2")
	assert.Empty(t, midModel.flashMessage,
		"stale submission #1 must not surface its error as the user thinks they're awaiting #2")

	// Now submission #2's real result arrives — must be processed.
	live := agentPickerEnqueueResultMsg{
		token: 2,
		jobID: 50,
		agent: "claude-code",
		err:   assert.AnError,
	}
	final, _ := midModel.handleAgentPickerEnqueueResultMsg(live)
	finalModel := final.(model)

	assert.Equal(t, uint64(0), finalModel.pendingEnqueueToken,
		"submission #2's real result must clear the pending token")
	assert.Contains(t, finalModel.flashMessage, "claude-code",
		"submission #2's flash should reflect its agent, not the stale submission #1's")
}

func TestAgentPicker_ConfirmGeneratesFreshToken(t *testing.T) {
	// Two confirm()s for the same job — even with the same selectedJobID
	// — must produce distinct tokens so the result handler can tell them
	// apart.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(99, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 99

	// First submission.
	m1, _ := pressKey(m, 'A')
	m2, _ := pressSpecial(m1, tea.KeyEnter)
	token1 := m2.pendingEnqueueToken
	require.NotZero(t, token1, "first submission must establish a pending token")

	// Second submission for the same job.
	m3, _ := pressKey(m2, 'A')
	m4, _ := pressSpecial(m3, tea.KeyEnter)
	token2 := m4.pendingEnqueueToken
	require.NotZero(t, token2, "second submission must establish a pending token")

	assert.NotEqual(t, token1, token2,
		"each submission must mint a fresh token so late results from earlier submissions can be dropped")
}

func TestAgentPicker_IndexOfAgentFallsBackToZero(t *testing.T) {
	names := agent.KnownAgentNames()
	assert.Equal(t, 0, indexOfAgent("nonexistent", names),
		"unknown agent name should default to index 0")
	assert.Equal(t, 0, indexOfAgent(names[0], names),
		"first known agent should be at index 0")
}

func TestAgentPicker_RefusesDirtyReviewByGitRef(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := makeReviewJob(7, withAgent("codex"), withRef("dirty"))
	job.JobType = storage.JobTypeDirty
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx = 0
	m.selectedJobID = 7

	result, _ := pressKey(m, 'A')

	assert.Equal(t, viewAgentPicker, result.currentView,
		"picker opens so the user sees the refusal message")
	assert.Empty(t, result.agentPickerAgents,
		"agent list should not be populated when refusing")
	assert.Contains(t, result.agentPickerErr, "dirty",
		"error should explain why")
}

func TestAgentPicker_RefusesDirtyReviewByJobType(t *testing.T) {
	// A row with JobType=="dirty" must be refused even when DiffContent
	// is nil (queue listing doesn't hydrate it) and git_ref is a SHA
	// rather than the literal "dirty" string. The job_type column is the
	// authoritative signal and dropping the picker through to enqueue
	// would build a non-dirty request the daemon would silently treat as
	// a single-commit review.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := makeReviewJob(31, withAgent("codex"), withRef("abc123"))
	job.JobType = storage.JobTypeDirty
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx = 0
	m.selectedJobID = 31

	result, _ := pressKey(m, 'A')

	assert.Equal(t, viewAgentPicker, result.currentView)
	assert.Empty(t, result.agentPickerAgents)
	assert.Contains(t, result.agentPickerErr, "dirty",
		"JobTypeDirty must trigger the dirty refusal even without DiffContent hydration")
}

func TestAgentPicker_RefusesDirtyReviewByDiffContentLegacy(t *testing.T) {
	// Legacy rows without job_type fall back to DiffContent-pointer / "dirty"
	// git_ref heuristic in ReviewJob.IsDirtyJob() — that path must still
	// trigger the picker's refusal.
	dirty := "diff --git a/foo b/foo\n+bar\n"
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := makeJob(8, withAgent("codex")) // no JobType set — exercise the legacy heuristic
	job.DiffContent = &dirty
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx = 0
	m.selectedJobID = 8

	result, _ := pressKey(m, 'A')

	assert.Equal(t, viewAgentPicker, result.currentView)
	assert.Empty(t, result.agentPickerAgents)
	assert.Contains(t, result.agentPickerErr, "dirty")
}

func TestAgentPicker_ConfirmRefusesIfJobBecameDirty(t *testing.T) {
	// Open the picker on a clean job; then the same job becomes dirty
	// (e.g. a queue refresh between A and Enter flips job_type). Confirm
	// re-checks eligibility and must refuse rather than emit a broken
	// enqueue. Real-world dirty promotion flips JobType, so the test
	// flips it explicitly.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeReviewJob(9, withAgent("codex"))}
	m.selectedIdx = 0
	m.selectedJobID = 9

	m2, _ := pressKey(m, 'A')
	require.Equal(t, viewAgentPicker, m2.currentView)

	m2.jobs[0].JobType = storage.JobTypeDirty
	m2.jobs[0].GitRef = "dirty"

	m3, cmd := pressSpecial(m2, tea.KeyEnter)
	assert.Equal(t, viewAgentPicker, m3.currentView,
		"picker should stay open on a dirty refusal at confirm time")
	assert.Contains(t, m3.agentPickerErr, "dirty")
	assert.Nil(t, cmd, "no enqueue command should be issued")
}

func TestAgentPicker_RefusesNonReviewJobTypes(t *testing.T) {
	// Each of these job types either carries extra payload (stored prompts,
	// since timestamps) that the picker can't forward, or has its own rerun
	// path (synthesis), so the picker must refuse.
	for _, jt := range []string{
		storage.JobTypeTask,
		storage.JobTypeCompact,
		storage.JobTypeFix,
		storage.JobTypeInsights,
		storage.JobTypeClassify,
		storage.JobTypeSynthesis,
	} {
		t.Run(jt, func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			job := makeJob(1, withAgent("codex"))
			job.JobType = jt
			m.jobs = []storage.ReviewJob{job}
			m.selectedIdx = 0
			m.selectedJobID = 1

			result, _ := pressKey(m, 'A')
			assert.Equal(t, viewAgentPicker, result.currentView,
				"picker opens so the user sees why it refused")
			assert.Empty(t, result.agentPickerAgents,
				"agent list should not be populated for non-review jobs")
			assert.NotEmpty(t, result.agentPickerErr, "must include a refusal reason")
		})
	}
}

func TestAgentPicker_RefusesPanelMembers(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := makeReviewJob(11, withAgent("codex"))
	job.PanelRole = storage.PanelRoleMember
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx = 0
	m.selectedJobID = 11

	result, _ := pressKey(m, 'A')

	assert.Equal(t, viewAgentPicker, result.currentView)
	assert.Empty(t, result.agentPickerAgents)
	assert.Contains(t, result.agentPickerErr, "synthesis",
		"refusal should point the user at the synthesis row")
}

func TestAgentPicker_PayloadPrefersWorktreePathOverRepoPath(t *testing.T) {
	job := makeReviewJob(1, withAgent("codex"), withRepoPath("/repos/main"))
	job.WorktreePath = "/repos/main-wt-feature-x"

	body := buildAgentPickerEnqueueBody(job, "gemini")

	assert.Equal(t, "/repos/main-wt-feature-x", body.RepoPath,
		"worktree jobs must pass the worktree path so the daemon detects the right checkout")
}

func TestAgentPicker_PayloadFallsBackToRepoPathWithoutWorktree(t *testing.T) {
	job := makeReviewJob(1, withAgent("codex"), withRepoPath("/repos/main"))

	body := buildAgentPickerEnqueueBody(job, "gemini")

	assert.Equal(t, "/repos/main", body.RepoPath,
		"non-worktree jobs continue to use RepoPath")
}

func TestAgentPicker_PayloadCopiesReviewContext(t *testing.T) {
	job := makeReviewJob(1,
		withAgent("codex"),
		withRepoPath("/repos/main"),
		withRef("abc123"),
		withBranch("feat/foo"),
		withReviewType("security"),
	)
	job.JobType = storage.JobTypeReview
	job.Reasoning = "thorough"
	job.MinSeverity = "high"
	job.Agentic = true
	job.OutputPrefix = "[panel-member-3] "
	job.RequestedModel = "claude-sonnet-4"
	job.RequestedProvider = "anthropic"

	body := buildAgentPickerEnqueueBody(job, "claude-code")

	assert.Equal(t, "claude-code", body.Agent)
	assert.Equal(t, "abc123", body.GitRef)
	assert.Equal(t, "feat/foo", body.Branch)
	assert.Equal(t, "security", body.ReviewType)
	assert.Equal(t, storage.JobTypeReview, body.JobType)
	assert.Equal(t, "thorough", body.Reasoning)
	assert.Equal(t, "high", body.MinSeverity)
	assert.True(t, body.Agentic)
	assert.Equal(t, "[panel-member-3] ", body.OutputPrefix,
		"OutputPrefix must be forwarded so panel-member prefixing isn't lost on re-run")
	assert.Equal(t, "claude-sonnet-4", body.Model,
		"explicit RequestedModel must travel as the daemon's model override")
	assert.Equal(t, "anthropic", body.Provider)
	assert.Equal(t, "none", body.Panel,
		"panel must be forced to none so the choice can't fan out into a panel run")
}

func TestAgentPicker_PayloadAlwaysSetsStrictAgent(t *testing.T) {
	// The picker is the explicit-agent UX: if the daemon can't honor the
	// pick, the user must be told. Falling back silently would silently
	// run the wrong agent.
	job := makeReviewJob(1, withAgent("codex"))

	body := buildAgentPickerEnqueueBody(job, "claude-code")

	assert.True(t, body.StrictAgent,
		"picker submissions must request strict-agent enforcement so the daemon errors on fallback instead of silently substituting")
}

func TestAgentPicker_PayloadOmitsResolvedModelWhenNotRequested(t *testing.T) {
	job := makeReviewJob(1, withAgent("codex"))
	// Effective values that came from defaulting — not explicit user requests.
	job.Model = "gpt-5"
	job.Provider = "openai"
	job.RequestedModel = ""
	job.RequestedProvider = ""

	body := buildAgentPickerEnqueueBody(job, "gemini")

	assert.Empty(t, body.Model,
		"resolved Model must not leak into the override field; let the new agent resolve its own default")
	assert.Empty(t, body.Provider)
}

func TestAgentPicker_ResolvesSelectedJobViaSideFetchedMembers(t *testing.T) {
	// A panel member can be side-fetched into m.panelMembers without sitting
	// in m.jobs. The picker refuses members on open, but the lookup path
	// must use selectedJob() (not just m.jobs) so the refusal message
	// reflects what the user actually selected — not "job no longer in
	// queue".
	m := newModel(localhostEndpoint, withExternalIODisabled())
	member := makeReviewJob(21, withAgent("codex"))
	member.PanelRole = storage.PanelRoleMember
	m.panelMembers = map[string][]storage.ReviewJob{
		"run-uuid-1": {member},
	}
	m.selectedJobID = 21
	m.currentView = viewQueue

	result, _ := m.handleAgentPickerOpenKey()
	updated := result.(model)

	assert.Equal(t, viewAgentPicker, updated.currentView)
	assert.Contains(t, updated.agentPickerErr, "synthesis",
		"refusal should be about panel membership, not 'job no longer in queue'")
}
