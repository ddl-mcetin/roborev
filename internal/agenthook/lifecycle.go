package agenthook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
)

const daemonRestartHint = "roborev agent-hook daemon restart"

type restartOutput struct {
	Stopped []int `json:"stopped,omitempty"`
	PID     int   `json:"pid"`
}

type daemonStatusOutput struct {
	Running bool                 `json:"running"`
	Count   int                  `json:"count"`
	PIDs    []int                `json:"pids,omitempty"`
	Records []daemonStatusRecord `json:"records,omitempty"`
}

type daemonStatusRecord struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version,omitempty"`
	Network   string    `json:"network,omitempty"`
	Address   string    `json:"address,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Reachable bool      `json:"reachable"`
	PingPID   int       `json:"ping_pid,omitempty"`
}

// RunDaemonStart starts a detached agent hook daemon, refusing to start a
// duplicate when one is already running.
func RunDaemonStart(stdout io.Writer) error {
	records, err := liveDaemonRecords()
	if err != nil {
		return err
	}
	switch len(records) {
	case 0:
	case 1:
		return writeDaemonStatus(stdout, records)
	default:
		return fmt.Errorf("multiple %s daemons are running (%v); use `%s` to replace them", ServiceName, pids(records), daemonRestartHint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exe, err := restartExecutable()
	if err != nil {
		return err
	}
	if err := startDetachedDaemonExecutable(ctx, exe); err != nil {
		return err
	}
	rec, err := waitForDaemon(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	return writeDaemonStatus(stdout, []kitdaemon.RuntimeRecord{rec})
}

// RunDaemonStatus prints live agent hook daemon records as JSON.
func RunDaemonStatus(stdout io.Writer) error {
	records, err := liveDaemonRecords()
	if err != nil {
		return err
	}
	return writeDaemonStatus(stdout, records)
}

// RunDaemonStop terminates every live agent hook daemon and reports the PIDs
// it stopped.
func RunDaemonStop(stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped, err := stopLiveDaemons(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Stopped []int `json:"stopped"`
	}{Stopped: stopped})
}

// RunDaemonRestart stops any live agent hook daemon and starts a fresh one from
// the caller's binary.
func RunDaemonRestart(stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stopped, err := stopLiveDaemons(ctx)
	if err != nil {
		return err
	}
	exe, err := restartExecutable()
	if err != nil {
		return err
	}
	if err := startDetachedDaemonExecutable(ctx, exe); err != nil {
		return err
	}
	rec, err := waitForDaemon(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(restartOutput{
		Stopped: stopped,
		PID:     rec.PID,
	})
}

func waitForDaemon(ctx context.Context, timeout time.Duration) (kitdaemon.RuntimeRecord, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		rec, _, ok, err := agentHookDaemonManager().Find(ctx)
		if err != nil {
			lastErr = err
		} else if ok {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return kitdaemon.RuntimeRecord{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("agent hook daemon failed to start within %s: %w", timeout, lastErr)
	}
	return kitdaemon.RuntimeRecord{}, fmt.Errorf("agent hook daemon failed to start within %s", timeout)
}

func writeDaemonStatus(stdout io.Writer, records []kitdaemon.RuntimeRecord) error {
	out := daemonStatusOutput{
		Running: len(records) > 0,
		Count:   len(records),
		PIDs:    pids(records),
		Records: make([]daemonStatusRecord, 0, len(records)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, rec := range records {
		status := daemonStatusRecord{
			PID:       rec.PID,
			Version:   rec.Version,
			Network:   rec.Network,
			Address:   rec.Address,
			StartedAt: rec.StartedAt,
		}
		info, err := kitdaemon.Probe(ctx, rec.Endpoint(), kitdaemon.ProbeOptions{
			ExpectedService: ServiceName,
			Timeout:         250 * time.Millisecond,
		})
		if err == nil {
			status.Reachable = true
			status.PingPID = info.PID
		}
		out.Records = append(out.Records, status)
	}
	return json.NewEncoder(stdout).Encode(out)
}

func pids(records []kitdaemon.RuntimeRecord) []int {
	ids := make([]int, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.PID)
	}
	return ids
}

func assertNoLiveDaemonRecords() error {
	records, err := liveDaemonRecords()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s daemon already running as pid %d; use `%s` to replace it",
		ServiceName,
		records[0].PID,
		daemonRestartHint,
	)
}

func liveDaemonRecords() ([]kitdaemon.RuntimeRecord, error) {
	records, err := runtimeStore().List()
	if err != nil {
		return nil, err
	}
	live := make([]kitdaemon.RuntimeRecord, 0, len(records))
	for _, rec := range records {
		if rec.Service != "" && rec.Service != ServiceName {
			continue
		}
		if rec.PID == os.Getpid() || !kitdaemon.ProcessAlive(rec.PID) {
			continue
		}
		live = append(live, rec)
	}
	return live, nil
}

func stopLiveDaemons(ctx context.Context) ([]int, error) {
	records, err := liveDaemonRecords()
	if err != nil {
		return nil, err
	}
	stopped := make([]int, 0, len(records))
	for _, rec := range records {
		process, err := os.FindProcess(rec.PID)
		if err != nil {
			return nil, fmt.Errorf("find agent hook daemon pid %d: %w", rec.PID, err)
		}
		if err := process.Signal(syscall.SIGTERM); err != nil {
			return nil, fmt.Errorf("stop agent hook daemon pid %d: %w", rec.PID, err)
		}
		stopped = append(stopped, rec.PID)
	}
	for {
		records, err := liveDaemonRecords()
		if err != nil {
			return nil, err
		}
		if len(records) == 0 {
			return stopped, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for agent hook daemon shutdown: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
