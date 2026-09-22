package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	defaultTaskSupplementPollInterval  = 5 * time.Second
	defaultTaskSupplementReadyInterval = 50 * time.Millisecond
	taskSupplementInjectTimeout        = 8 * time.Second
	taskSupplementAckTimeout           = 5 * time.Second
)

// taskSupplementSignals routes best-effort WebSocket hints to one exact
// in-flight task. A one-slot channel coalesces duplicate hints. Creating the
// slot before provider launch preserves a hint sent after the server marks the
// task running but before Codex reports turn/started.
type taskSupplementSignals struct {
	mu     sync.Mutex
	byTask map[string]chan struct{}
}

func newTaskSupplementSignals() *taskSupplementSignals {
	return &taskSupplementSignals{byTask: make(map[string]chan struct{})}
}

func (s *taskSupplementSignals) subscribe(taskID string) (<-chan struct{}, func()) {
	s.mu.Lock()
	ch := s.byTask[taskID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.byTask[taskID] = ch
	}
	s.mu.Unlock()
	return ch, func() { s.clear(taskID, ch) }
}

func (s *taskSupplementSignals) notify(taskID string) {
	if s == nil || taskID == "" {
		return
	}
	s.mu.Lock()
	ch := s.byTask[taskID]
	if ch == nil {
		// A negotiated task registers its slot before provider launch. Ignore
		// stale or unnegotiated hints instead of retaining an unowned channel.
		s.mu.Unlock()
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	s.mu.Unlock()
}

func (s *taskSupplementSignals) clear(taskID string, expected ...chan struct{}) {
	if s == nil || taskID == "" {
		return
	}
	s.mu.Lock()
	if len(expected) == 0 || s.byTask[taskID] == expected[0] {
		delete(s.byTask, taskID)
	}
	s.mu.Unlock()
}

func (d *Daemon) taskSupplementWakeup(taskID string) (<-chan struct{}, func()) {
	d.taskSupplementSignalsInitMu.Lock()
	if d.taskSupplementSignals == nil {
		d.taskSupplementSignals = newTaskSupplementSignals()
	}
	signals := d.taskSupplementSignals
	d.taskSupplementSignalsInitMu.Unlock()
	return signals.subscribe(taskID)
}

func (d *Daemon) signalTaskSupplementWakeup(taskID string) {
	d.taskSupplementSignalsInitMu.Lock()
	if d.taskSupplementSignals == nil {
		// No negotiated run has registered a slot yet. The durable row will be
		// found by the bounded poll after a slot exists, so there is no reason to
		// allocate state for an unsupported, old, or already-finished task.
		d.taskSupplementSignalsInitMu.Unlock()
		return
	}
	signals := d.taskSupplementSignals
	d.taskSupplementSignalsInitMu.Unlock()
	signals.notify(taskID)
}

func (d *Daemon) clearTaskSupplementWakeup(taskID string) {
	d.taskSupplementSignalsInitMu.Lock()
	signals := d.taskSupplementSignals
	d.taskSupplementSignalsInitMu.Unlock()
	if signals != nil {
		signals.clear(taskID)
	}
}

func (d *Daemon) effectiveTaskSupplementPollInterval() time.Duration {
	if d.taskSupplementPollInterval > 0 {
		return d.taskSupplementPollInterval
	}
	return defaultTaskSupplementPollInterval
}

func (d *Daemon) effectiveTaskSupplementReadyInterval() time.Duration {
	if d.taskSupplementReadyInterval > 0 {
		return d.taskSupplementReadyInterval
	}
	return defaultTaskSupplementReadyInterval
}

func waitTaskSupplement(ctx context.Context, wakeup <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wakeup:
		return true
	case <-timer.C:
		return true
	}
}

func taskSupplementEndpointUnsupported(err error) bool {
	var reqErr *requestError
	return errors.As(err, &reqErr) && (reqErr.StatusCode == http.StatusNotFound || reqErr.StatusCode == http.StatusPreconditionFailed)
}

func taskSupplementFailureReason(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.TaskSupplementFailureTimeout
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return protocol.TaskSupplementFailureTurnEnded
	}
	if strings.Contains(strings.ToLower(err.Error()), "turn") && strings.Contains(strings.ToLower(err.Error()), "not started") {
		return protocol.TaskSupplementFailureTurnNotStarted
	}
	return protocol.TaskSupplementFailureProviderRejected
}

// runTaskSupplementLoop serially claims and acknowledges durable additions for
// one negotiated run. It performs no HTTP request until Codex confirms a live
// turn, wakes immediately on a content-free WebSocket hint, and otherwise uses
// the same five-second cadence as task cancellation polling.
func (d *Daemon) runTaskSupplementLoop(ctx, parentCtx context.Context, session *agent.Session, taskID string, wakeup <-chan struct{}, taskLog *slog.Logger) {
	if session == nil || session.Supplement == nil || session.SupplementReady == nil {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if !session.SupplementReady() {
			if !waitTaskSupplement(ctx, wakeup, d.effectiveTaskSupplementReadyInterval()) {
				return
			}
			continue
		}

		claimCtx, cancelClaim := context.WithTimeout(ctx, 3*time.Second)
		supplement, claimErr := d.client.ClaimTaskSupplement(claimCtx, taskID)
		cancelClaim()
		if claimErr != nil {
			if taskSupplementEndpointUnsupported(claimErr) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			taskLog.Debug("additional message claim failed", "error", claimErr)
			if !waitTaskSupplement(ctx, wakeup, d.effectiveTaskSupplementPollInterval()) {
				return
			}
			continue
		}
		if supplement == nil {
			if !waitTaskSupplement(ctx, wakeup, d.effectiveTaskSupplementPollInterval()) {
				return
			}
			continue
		}

		injectCtx, cancelInject := context.WithTimeout(ctx, taskSupplementInjectTimeout)
		injectErr := session.Supplement(injectCtx, formatTaskSupplementInstruction(supplement.AuthorName, supplement.Content))
		reason := taskSupplementFailureReason(injectCtx, injectErr)
		cancelInject()
		if injectErr != nil {
			// Raw provider/Go diagnostics remain local. Workspace-visible state is
			// restricted to the stable reason code sent below.
			taskLog.Warn("additional message injection failed", "comment_id", supplement.CommentID, "reason", reason, "error", injectErr)
		}

		ackCtx, cancelAck := context.WithTimeout(context.WithoutCancel(parentCtx), taskSupplementAckTimeout)
		ackErr := d.client.AckTaskSupplement(ackCtx, taskID, supplement.CommentID, injectErr == nil, reason)
		cancelAck()
		if ackErr != nil {
			taskLog.Warn("additional message acknowledgement failed", "comment_id", supplement.CommentID, "error", ackErr)
		}
		// Do not sleep after a claim: drain already-ordered pending rows one at a
		// time. When empty, the branch above returns to the bounded wait.
	}
}
