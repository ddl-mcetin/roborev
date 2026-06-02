package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

type fakeTelemetryClient struct {
	enabled    bool
	event      string
	properties map[string]any
}

func (f *fakeTelemetryClient) Capture(event string, properties map[string]any) error {
	f.event = event
	f.properties = properties
	return nil
}

func (f *fakeTelemetryClient) Close() error { return nil }

func (f *fakeTelemetryClient) Enabled() bool { return f.enabled }

func TestCaptureDaemonStartedTelemetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, db, tmpDir := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)

	_, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(err)

	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 4
	cfg.Sync.Enabled = true
	cfg.CI.Enabled = true
	cfg.AutoDesignReview.Enabled = true

	server.captureDaemonStartedTelemetry(cfg)

	assert.Equal("daemon_started", client.event)
	assert.Equal(1, client.properties["repo_count"])
	assert.Equal(4, client.properties["worker_count"])
	assert.Equal(true, client.properties["sync_enabled"])
	assert.Equal(true, client.properties["ci_enabled"])
	assert.Equal(true, client.properties["auto_design_enabled"])
}

func TestCaptureDaemonStartedTelemetryDisabledNoops(t *testing.T) {
	assert := assert.New(t)

	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: false}
	server.SetTelemetry(client)

	server.captureDaemonStartedTelemetry(config.DefaultConfig())

	assert.Empty(client.event)
	assert.Nil(client.properties)
}
