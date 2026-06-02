package telemetry

import (
	"path/filepath"
	"testing"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type fakePostHogClient struct {
	message posthog.Message
}

func (f *fakePostHogClient) Enqueue(message posthog.Message) error {
	f.message = message
	return nil
}

func (f *fakePostHogClient) Close() error { return nil }

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestNewReporterDisabledByEnvDoesNotCreateInstallID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(EnabledEnv, "0")
	database := openTestDB(t)

	reporter, err := NewReporter(Options{Database: database})
	require.NoError(err)

	assert.False(reporter.Enabled())
	value, err := database.GetSyncState(installIDMetadataKey)
	require.NoError(err)
	assert.Empty(value)
}

func TestLoadOrCreateInstallIDIsStableAndAnonymous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)

	first, err := loadOrCreateInstallID(database)
	require.NoError(err)
	second, err := loadOrCreateInstallID(database)
	require.NoError(err)

	assert.Len(first, 32)
	assert.Equal(first, second)

	stored, err := database.GetSyncState(installIDMetadataKey)
	require.NoError(err)
	assert.Equal(first, stored)
}

func TestReporterCaptureUsesAnonymousDistinctID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}

	err := reporter.Capture(EventDaemonStarted, map[string]any{
		"$geoip_disable": false,
		"distinct_id":    "user-provided",
		"repo":           "owner/name",
		"repo_count":     3,
		"sync_enabled":   true,
	})
	require.NoError(err)

	capture, ok := client.message.(posthog.Capture)
	require.True(ok)
	assert.Equal("anonymous-install-id", capture.DistinctId)
	assert.Equal(EventDaemonStarted, capture.Event)
	assert.Equal(3, capture.Properties["repo_count"])
	assert.Equal(true, capture.Properties["sync_enabled"])
	assert.NotContains(capture.Properties, "distinct_id")
	assert.NotContains(capture.Properties, "repo")
	assert.True(capture.Properties["$geoip_disable"].(bool))
}

func TestReporterCaptureRejectsUnsupportedEvents(t *testing.T) {
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}

	err := reporter.Capture("repo_opened", map[string]any{"repo_count": 1})
	require.ErrorIs(err, ErrUnsupportedEvent)
}

func TestReporterCaptureAllowsDailyActiveEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}

	err := reporter.Capture(EventDaemonActiveDaily, map[string]any{
		"repo_count":   3,
		"sync_enabled": true,
	})
	require.NoError(err)

	capture, ok := client.message.(posthog.Capture)
	require.True(ok)
	assert.Equal(EventDaemonActiveDaily, capture.Event)
	assert.Equal(3, capture.Properties["repo_count"])
	assert.Equal(true, capture.Properties["sync_enabled"])
}

func TestReporterCaptureDropsUnsafePropertyValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}

	err := reporter.Capture(EventDaemonStarted, map[string]any{
		"repo_count":   "owner/repo",
		"sync_enabled": "yes",
	})
	require.NoError(err)

	capture, ok := client.message.(posthog.Capture)
	require.True(ok)
	assert.NotContains(capture.Properties, "repo_count")
	assert.NotContains(capture.Properties, "sync_enabled")
	assert.True(capture.Properties["$geoip_disable"].(bool))
}
