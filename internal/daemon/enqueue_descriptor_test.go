package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// enqueueViaHTTP posts an enqueue request through the real HTTP handler and
// decodes the created job. It asserts a 201 so the regression tests fail loudly
// if the refactor changes the response shape or status.
func enqueueViaHTTP(t *testing.T, server *Server, body EnqueueRequest) storage.ReviewJob {
	t.Helper()

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", body)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	return job
}

// TestEnqueueSingleCommitUnchanged pins the single-commit enqueue path: the
// stored job must carry CommitID, the frozen SHA, a non-empty PatchID, and the
// commit subject, with the resolved agent/model/reasoning preserved verbatim.
func TestEnqueueSingleCommitUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	assert.Equal(storage.JobTypeReview, stored.JobType)
	assert.Equal(sha, stored.GitRef)
	assert.NotZero(stored.CommitIDValue(), "single-commit job must reference a commit row")
	assert.NotEmpty(stored.PatchID, "single-commit job must record a patch id")
	assert.Equal("test", stored.Agent)
	assert.NotEmpty(stored.Reasoning, "reasoning must be resolved")
	assert.Empty(stored.Prompt, "single-commit review must not store a prompt")

	// CommitSubject is set on the returned job (not persisted on review_jobs),
	// so assert against the HTTP response value.
	assert.Equal("add a", job.CommitSubject)
}

// TestEnqueueDirtyUnchanged pins the dirty enqueue path: the stored job must be
// classified as dirty and preserve the supplied diff content.
func TestEnqueueDirtyUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	diff := "diff --git a/x b/x\n+change\n"
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:    repo.Path(),
		GitRef:      "dirty",
		Agent:       "test",
		DiffContent: diff,
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeDirty, stored.JobType)
	assert.Equal("dirty", stored.GitRef)
	assert.NotZero(stored.CommitIDValue(), "dirty job stores base HEAD for session reuse")

	// GetJobByID does not hydrate diff_content; the worker reads it via ClaimJob,
	// so verify the stored diff survives through the worker's view.
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NotNil(t, claimed.DiffContent)
	assert.Equal(diff, *claimed.DiffContent)
}

// TestEnqueueRangeUnchanged pins the range enqueue path: the symbolic
// "<a>..<b>" request is frozen to "<sha>..<sha>" and classified as a range.
func TestEnqueueRangeUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	firstSHA := repo.CommitFile("a.txt", "a", "add a")
	secondSHA := repo.CommitFile("b.txt", "b", "add b")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD~1..HEAD",
		Agent:    "test",
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	assert.Equal(storage.JobTypeRange, stored.JobType)
	assert.Equal(firstSHA+".."+secondSHA, stored.GitRef)
	assert.Zero(stored.CommitIDValue(), "range job must not reference a single commit row")
}

// TestEnqueuePromptJobUnchanged pins the stored-prompt enqueue path: the
// finding-driven regression proving Prompt/OutputPrefix/Agentic/Label survive
// the refactor and the prompt is not flagged prebuilt.
func TestEnqueuePromptJobUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:     repo.Path(),
		GitRef:       "task-label",
		Agent:        "test",
		CustomPrompt: "do X",
		OutputPrefix: "P",
		Agentic:      true,
		JobType:      storage.JobTypeTask,
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeTask, stored.JobType)
	assert.Equal("do X", stored.Prompt)
	assert.True(stored.Agentic)
	// Label drives the git_ref display value for task jobs.
	assert.Equal("task-label", stored.GitRef)
	assert.Zero(stored.CommitIDValue(), "prompt job must not reference a commit row")

	// GetJobByID omits output_prefix / prompt_prebuilt; the worker reads them via
	// ClaimJob, so verify them through the worker's view.
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	assert.Equal("P", claimed.OutputPrefix)
	assert.False(claimed.PromptPrebuilt, "humaEnqueue never marks prompts prebuilt")
}

// TestEnqueueResponseUnchanged pins the HTTP response shape: a bare
// storage.ReviewJob (status 201), decodable directly into the model.
func TestEnqueueResponseUnchanged(t *testing.T) {
	server, _, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.True(t, strings.HasPrefix(strings.TrimSpace(w.Body.String()), "{"),
		"response must be a bare JSON object, not a wrapper")

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	assert.Positive(t, job.ID)
	assert.Equal(t, "test", job.Agent)
	assert.Equal(t, storage.JobStatusQueued, job.Status)
}

// TestEnqueueCodeReviewPreservesOutputPrefix guards a regression seen in the
// TUI agent-picker rerun flow: code-review descriptors (single-commit, range,
// dirty) used to drop req.OutputPrefix on the floor because only
// descriptorForPrompt copied it through. The picker re-enqueues with the
// original job's output_prefix to preserve panel-member prefixing across
// agent switches, so the persisted job must carry the prefix forward.
func TestEnqueueCodeReviewPreservesOutputPrefix(t *testing.T) {
	t.Run("single-commit", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.txt", "a", "add a")

		job := enqueueViaHTTP(t, server, EnqueueRequest{
			RepoPath:     repo.Path(),
			GitRef:       "HEAD",
			Agent:        "test",
			OutputPrefix: "[member-3] ",
		})

		claimed, err := db.ClaimJob("worker")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		assert.Equal(t, "[member-3] ", claimed.OutputPrefix,
			"single-commit review must preserve OutputPrefix end-to-end")
	})

	t.Run("range", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.txt", "a", "add a")
		repo.CommitFile("b.txt", "b", "add b")

		job := enqueueViaHTTP(t, server, EnqueueRequest{
			RepoPath:     repo.Path(),
			GitRef:       "HEAD~1..HEAD",
			Agent:        "test",
			OutputPrefix: "[member-3] ",
		})

		claimed, err := db.ClaimJob("worker")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		assert.Equal(t, "[member-3] ", claimed.OutputPrefix,
			"range review must preserve OutputPrefix end-to-end")
	})

	t.Run("dirty", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.txt", "a", "add a")

		job := enqueueViaHTTP(t, server, EnqueueRequest{
			RepoPath:     repo.Path(),
			GitRef:       "dirty",
			Agent:        "test",
			DiffContent:  "diff --git a/x b/x\n+change\n",
			OutputPrefix: "[member-3] ",
		})

		claimed, err := db.ClaimJob("worker")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		assert.Equal(t, "[member-3] ", claimed.OutputPrefix,
			"dirty review must preserve OutputPrefix end-to-end")
	})
}

// TestEnqueueStrictAgentRejectsUnavailable guards the TUI agent-picker
// contract: when strict_agent=true and the requested agent is not actually
// available (e.g. unregistered, uninstalled, or overridden by workflow
// config), /api/enqueue must 400 rather than silently fall back to a
// different agent. The picker is an explicit-agent UX; receiving a
// substituted agent without notice would defeat the whole point.
func TestEnqueueStrictAgentRejectsUnavailable(t *testing.T) {
	// Make the test independent of the host's installed CLIs. The agent
	// registry is package-global, so:
	//   1. Override "pi" with an unavailableSynthesisCommandAgent whose
	//      command name is a fixed token that can never be on PATH —
	//      IsAvailable("pi") deterministically returns false regardless
	//      of whether the dev/CI box has a pi binary.
	//   2. Override "codex" (fallback rank 1) with a FakeAgent so the
	//      fallback chain has at least one always-available agent and
	//      the daemon doesn't 503 before reaching the strict-agent gate.
	//   3. Restore both originals via t.Cleanup so other tests in the
	//      package see normal registry state.
	originalPi, err := agent.Get("pi")
	require.NoError(t, err, "pi must be a known agent")
	agent.Register(&unavailableSynthesisCommandAgent{
		name: "pi", command: "test-pi-not-on-path-d9e7c1b3",
	})
	t.Cleanup(func() { agent.Register(originalPi) })

	originalCodex, err := agent.Get("codex")
	require.NoError(t, err, "codex must be a known agent")
	agent.Register(&agent.FakeAgent{NameStr: "codex"})
	t.Cleanup(func() { agent.Register(originalCodex) })

	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:    repo.Path(),
		GitRef:      "HEAD",
		Agent:       "pi",
		StrictAgent: true,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "unavailable",
		"error must explain why the agent was rejected")
	assert.Contains(t, w.Body.String(), "pi",
		"error should name the rejected agent so the user knows what to fix")

	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs, "strict-agent rejection must not create a job")
}

// TestEnqueueStrictAgentAllowsHonoredAgent ensures strict_agent isn't a hammer:
// when the requested agent IS available, enqueue proceeds normally.
func TestEnqueueStrictAgentAllowsHonoredAgent(t *testing.T) {
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:    repo.Path(),
		GitRef:      "HEAD",
		Agent:       "test",
		StrictAgent: true,
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "test", stored.Agent,
		"strict_agent with an available agent must persist exactly that agent")
}

// TestEnqueueExcludedCommitSkips pins the single-commit skip path: when the HEAD
// commit message matches an excluded pattern, buildTargetDescriptor returns the
// 200 Skipped *RawJSONOutput early return instead of a job. This is the
// skip-branch the descriptor refactor must preserve, distinct from the 201 paths.
func TestEnqueueExcludedCommitSkips(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", "excluded_commit_patterns = [\"skipme\"]\n")
	repo.CommitFile("a.txt", "a", "skipme: trivial change")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp EnqueueSkippedResponse
	testutil.DecodeJSON(t, w, &resp)
	assert.True(resp.Skipped, "excluded commit must report skipped")
	assert.Contains(resp.Reason, "excluded pattern")

	// A skipped enqueue must not create any job row.
	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Empty(jobs, "skipped enqueue must not create a job")
}
