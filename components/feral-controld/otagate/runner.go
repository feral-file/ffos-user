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

	// unitLivenessInterval is how often the tail cross-checks that the updater
	// unit is still running. The log tail alone cannot detect an updater that
	// dies without writing an id-tagged terminal line (SIGKILL, OOM-kill,
	// systemctl stop): the tail would EOF-poll forever, holding the gate's
	// single-flight key and permanently wedging every later update AND the
	// mandatory pre-claim gate. This watch is the bound that guarantees Run
	// always terminates.
	unitLivenessInterval = 5 * time.Second

	// unitDeadGrace is how long after the unit is first seen dead the tail keeps
	// reading before giving up, so a terminal line still being flushed to the
	// log is not lost to a race between the unit exiting and its last write.
	unitDeadGrace = 10 * time.Second
)

// UpdateRunner spawns the OTA updater and blocks until it completes (returns nil
// after reaching 100%) or fails (returns an error carrying the raw updater
// message so the retry ladder can classify it). onProgress receives the parsed
// percent and human-readable message for each progress line; pct is -1 when the
// line carried no percent field (e.g. a "Preparing" message). It may be nil.
//
// This is the injectable seam the gate's table tests replace with a fake, and
// the boundary behind which the real systemd-unit + log-tail mechanism lives.
type UpdateRunner interface {
	Run(ctx context.Context, onProgress func(pct int, message string)) error
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

	// unitActive reports whether the transient updater unit is still running; a
	// seam so tests can script the unit dying without systemd. nil means "always
	// alive" (the pure line-parsing tests); production wires systemctl.
	unitActive func(ctx context.Context, unit string) bool
}

// NewSystemdRunner returns the production UpdateRunner.
func NewSystemdRunner(exec wrapper.Exec, clock wrapper.Clock, logger *zap.Logger) UpdateRunner {
	r := &systemdRunner{
		exec:    exec,
		clock:   clock,
		logger:  logger,
		logPath: updaterLogFile,
		//nolint:gosec // path is the fixed in-image log path (updaterLogFile) or a test seam, never user input
		openLog: func(path string) (io.ReadCloser, error) { return os.Open(path) },
	}
	r.unitActive = r.systemdUnitActive
	return r
}

// systemdUnitActive is the production liveness probe behind the unitActive seam.
// A unit mid-run reports active/activating; inactive/failed/deactivating means
// the updater process is gone. Any systemctl failure biases to "alive" so a
// transient exec hiccup never aborts a healthy update.
func (r *systemdRunner) systemdUnitActive(ctx context.Context, unit string) bool {
	out, err := r.exec.CommandContext(ctx,
		"systemctl", "show", "-p", "ActiveState", "--value", unit).CombinedOutput()
	if err != nil {
		return true
	}
	switch strings.TrimSpace(string(out)) {
	case "inactive", "failed", "deactivating":
		return false
	}
	return true
}

func (r *systemdRunner) Run(ctx context.Context, onProgress func(int, string)) (retErr error) {
	// A per-run id lets us ignore stale lines from an earlier updater run still
	// present in the append-only log. Ported from updater.rs run id.
	id := fmt.Sprintf("controld-%d", rand.Int63n(int64(^uint64(0)>>1))+1) //nolint:gosec
	unit := fmt.Sprintf("feral-updater-run@%s.service", id)

	// 1. Stop feral-watchdog to avoid it fighting the update. Best-effort.
	_ = r.exec.CommandContext(ctx, "systemctl", "--user", "stop", "feral-watchdog.service").Run()

	// Restart the watchdog on EVERY exit path out of this run, success included.
	// Restart=always does not resurrect an explicitly-stopped unit and the only
	// other start is .start-services.sh at boot, so a run that returns without
	// this would leave the kiosk watchdog dead on a device that runs unattended
	// for months. Success is NOT exempted: on success the updater unit reboots
	// into the new build and that reboot restarts the watchdog anyway, so this
	// restart is a harmless no-op in the common case — but if the reboot is
	// deferred, staged, or fails, this is the only thing that keeps the watchdog
	// alive on the still-running old build (the F5 gap). The brief window where
	// the watchdog runs before an imminent reboot cannot harm anything the reboot
	// doesn't already tear down. context.Background(): the failure may BE ctx's
	// cancellation, which must not also skip the restart.
	defer func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.exec.CommandContext(restartCtx,
			"systemctl", "--user", "start", "feral-watchdog.service").Run(); err != nil && r.logger != nil {
			r.logger.Warn("OTA runner: failed to restart feral-watchdog after update run", zap.Error(err))
		}
	}()

	// 2. Start the transient updater unit and wait for the start command itself.
	startErr := r.exec.CommandContext(ctx, "systemctl", "start", unit).Run()
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

	// 4. Tail and interpret each line until completion, error, unit death, or
	// ctx cancel.
	return r.tail(ctx, rc, id, unit, onProgress)
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

func (r *systemdRunner) tail(ctx context.Context, rc io.Reader, id, unit string, onProgress func(int, string)) error {
	reader := bufio.NewReader(rc)
	receivedProgress := false

	// Unit-liveness watch: the log alone cannot signal an updater killed without
	// an id-tagged terminal line (SIGKILL/OOM/systemctl stop), so the EOF branch
	// periodically probes the unit and, once it is seen dead, keeps draining the
	// log for unitDeadGrace before declaring the run lost. deadSince is zero
	// while the unit is (believed) alive.
	nextLiveness := r.clock.Now().Add(unitLivenessInterval)
	var deadSince time.Time

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
					pct := -1
					if evt.hasPct {
						pct = evt.pct
					}
					onProgress(pct, evt.message)
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
				now := r.clock.Now()
				if r.unitActive != nil && !now.Before(nextLiveness) {
					nextLiveness = now.Add(unitLivenessInterval)
					if r.unitActive(ctx, unit) {
						deadSince = time.Time{}
					} else if deadSince.IsZero() {
						deadSince = now
					}
				}
				if !deadSince.IsZero() && now.Sub(deadSince) >= unitDeadGrace {
					// Wording is load-bearing: classifyUpdaterMessage treats this
					// exact substring as transient (a killed updater is retryable).
					//nolint:staticcheck // ST1005: intentional to preserve the classifier contract
					return fmt.Errorf("updater service exited without reporting completion")
				}
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
	kind updaterLineKind
	pct  int
	// hasPct distinguishes a real "progress=0" from a progress line that carried
	// no percent field (a message-only "Preparing" line). The tail forwards -1 for
	// the latter so a downstream UI does not paint a misleading 0%.
	hasPct  bool
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
			evt.hasPct = true
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
