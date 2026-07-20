package otagate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	// updaterLogFile is tailed for this run's progress/error lines. Ported from
	// feral-setupd constant::UPDATER_PROCESS_LOG_FILE.
	updaterLogFile = "/var/log/updaterd.log"

	// updaterLogOpenTimeout / updaterLogOpenRetry bound how long we wait for the
	// transient updater unit to create its log file. Ported from
	// updater.rs::run_update_and_send (1 minute total, retry every 5s).
	updaterLogOpenTimeout = 60 * time.Second
	updaterLogOpenRetry   = 5 * time.Second

	// updaterLogPollInterval is the tail poll cadence when the file has no new
	// line yet. Ported from updater.rs (200ms).
	updaterLogPollInterval = 200 * time.Millisecond
)

// UpdateRunner spawns the OTA updater and blocks until it completes (returns nil
// after reaching 100%) or fails (returns an error carrying the raw updater
// message so the retry ladder can classify it). onProgress receives each
// human-readable progress string for a future UI surface; it may be nil.
//
// This is the injectable seam the gate's table tests replace with a fake, and
// the boundary behind which the real systemd-unit + log-tail mechanism lives.
type UpdateRunner interface {
	Run(ctx context.Context, onProgress func(progress string)) error
}

// systemdRunner replicates feral-setupd's updater.rs spawn/monitor mechanism:
// stop the watchdog, start the transient feral-updater-run@{id}.service unit,
// then tail /var/log/updaterd.log for lines tagged with this run's id.
type systemdRunner struct {
	exec   wrapper.Exec
	clock  wrapper.Clock
	logger *zap.Logger

	// logPath and openLog are seams so the tail loop can be pointed at a test
	// file; production uses updaterLogFile and os.Open.
	logPath string
	openLog func(path string) (io.ReadCloser, error)
}

// NewSystemdRunner returns the production UpdateRunner.
func NewSystemdRunner(exec wrapper.Exec, clock wrapper.Clock, logger *zap.Logger) UpdateRunner {
	return &systemdRunner{
		exec:    exec,
		clock:   clock,
		logger:  logger,
		logPath: updaterLogFile,
		//nolint:gosec // path is the fixed in-image log path (updaterLogFile) or a test seam, never user input
		openLog: func(path string) (io.ReadCloser, error) { return os.Open(path) },
	}
}

func (r *systemdRunner) Run(ctx context.Context, onProgress func(string)) error {
	// A per-run id lets us ignore stale lines from an earlier updater run still
	// present in the append-only log. Ported from updater.rs run id.
	id := fmt.Sprintf("controld-%d", rand.Int63n(int64(^uint64(0)>>1))+1) //nolint:gosec

	// 1. Stop feral-watchdog to avoid it fighting the update. Best-effort.
	_ = r.exec.CommandContext(ctx, "systemctl", "--user", "stop", "feral-watchdog.service").Run()

	// 2. Start the transient updater unit and wait for the start command itself.
	startErr := r.exec.CommandContext(ctx,
		"systemctl", "start", fmt.Sprintf("feral-updater-run@%s.service", id)).Run()
	if startErr != nil {
		// Capitalization is load-bearing: classifyUpdaterMessage matches the exact
		// "Failed to start updater service" substring ported from feral-setupd.
		//nolint:staticcheck // ST1005: intentional to preserve the classifier contract
		return fmt.Errorf("Failed to start updater service: %w", startErr)
	}

	// 3. Open the log file with the bounded retry the unit needs to create it.
	rc, err := r.openLogWithRetry(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	// 4. Tail and interpret each line until completion, error, or ctx cancel.
	return r.tail(ctx, rc, id, onProgress)
}

func (r *systemdRunner) openLogWithRetry(ctx context.Context) (io.ReadCloser, error) {
	deadline := r.clock.Now().Add(updaterLogOpenTimeout)
	for {
		rc, err := r.openLog(r.logPath)
		if err == nil {
			return rc, nil
		}
		if !r.clock.Now().Before(deadline) {
			// Capitalization is load-bearing: classifyUpdaterMessage matches the exact
			// "Failed to open /var/log/updaterd.log" substring ported from feral-setupd.
			//nolint:staticcheck // ST1005: intentional to preserve the classifier contract
			return nil, fmt.Errorf("Failed to open %s after %s: %w",
				r.logPath, updaterLogOpenTimeout, err)
		}
		if serr := r.clock.SleepContext(ctx, updaterLogOpenRetry); serr != nil {
			return nil, serr
		}
	}
}

func (r *systemdRunner) tail(ctx context.Context, rc io.Reader, id string, onProgress func(string)) error {
	reader := bufio.NewReader(rc)
	receivedProgress := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if line != "" {
			evt := parseUpdaterLine(line, id)
			switch evt.kind {
			case updaterProgress:
				receivedProgress = true
				if onProgress != nil {
					onProgress(evt.message)
				}
				if evt.pct == 100 {
					return nil
				}
			case updaterError:
				return fmt.Errorf("%s", evt.message)
			case updaterOther:
				// Not our line, or an INFO line: keep tailing.
			}
		}
		if err != nil {
			if err == io.EOF {
				// No more data yet; wait for the updater to append more.
				if serr := r.clock.SleepContext(ctx, updaterLogPollInterval); serr != nil {
					return serr
				}
				continue
			}
			// A read error before any progress mirrors the Rust "closed channel
			// without progress" case (transient).
			if !receivedProgress {
				return fmt.Errorf("updater closed channel without sending progress")
			}
			return err
		}
	}
}

// updaterLineKind categorizes a parsed updaterd.log line.
type updaterLineKind int

const (
	updaterOther updaterLineKind = iota
	updaterProgress
	updaterError
)

type updaterEvent struct {
	kind    updaterLineKind
	pct     int
	message string
}

var (
	progressValueRe = regexp.MustCompile(`progress=(\d+)`)
	messageRe       = regexp.MustCompile(`message="([^"]*)"`)
)

// parseUpdaterLine interprets one updaterd.log line for the given run id.
//
// Ported from updater.rs::run_update_and_send tail logic: lines not tagged with
// this run's id are ignored; "[PROGRESS]" lines yield a percent + message (100%
// is terminal); "[ERROR]" lines yield the message (or "Unknown error occurred"
// when no message field is present).
func parseUpdaterLine(line, id string) updaterEvent {
	if id != "" && !strings.Contains(line, "id="+id) {
		return updaterEvent{kind: updaterOther}
	}

	switch {
	case strings.Contains(line, "[PROGRESS]"):
		evt := updaterEvent{kind: updaterProgress}
		payload := ""
		if m := progressValueRe.FindStringSubmatch(line); m != nil {
			pct, _ := strconv.Atoi(m[1])
			evt.pct = pct
			payload = m[1] + "%"
		}
		if m := messageRe.FindStringSubmatch(line); m != nil {
			if payload != "" {
				payload += " - " + m[1]
			} else {
				payload = m[1]
			}
		}
		evt.message = payload
		return evt

	case strings.Contains(line, "[ERROR]"):
		msg := "Unknown error occurred"
		if m := messageRe.FindStringSubmatch(line); m != nil {
			msg = m[1]
		}
		return updaterEvent{kind: updaterError, message: msg}

	default:
		return updaterEvent{kind: updaterOther}
	}
}
