package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestFormatTaskSupplementInstructionPreservesOriginalGoal(t *testing.T) {
	got := formatTaskSupplementInstruction("  Ada   Lovelace ", "also include a rollback note")
	for _, want := range []string{
		"additional guidance for the same active task",
		"Preserve and complete the original objective",
		"single final response",
		"only if the human explicitly asks",
		`Human "Ada Lovelace"`,
		"also include a rollback note",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction missing %q:\n%s", want, got)
		}
	}
}

func taskSupplementTestDaemon(t *testing.T, handler http.HandlerFunc) *Daemon {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Daemon{
		client:                      NewClient(server.URL),
		logger:                      slog.New(slog.NewTextHandler(io.Discard, nil)),
		taskSupplementSignals:       newTaskSupplementSignals(),
		taskSupplementPollInterval:  10 * time.Millisecond,
		taskSupplementReadyInterval: 5 * time.Millisecond,
	}
}

func TestTaskSupplementLoopWaitsForTurnReadyBeforeClaim(t *testing.T) {
	var claims atomic.Int32
	d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		claims.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	var ready atomic.Bool
	session := &agent.Session{
		SupplementReady: ready.Load,
		Supplement:      func(context.Context, string) error { return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wakeup, unsubscribe := d.taskSupplementWakeup("task-ready")
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runTaskSupplementLoop(ctx, ctx, session, "task-ready", wakeup, d.logger)
	}()
	time.Sleep(30 * time.Millisecond)
	if got := claims.Load(); got != 0 {
		t.Fatalf("claims before turn ready = %d, want 0", got)
	}
	ready.Store(true)
	d.signalTaskSupplementWakeup("task-ready")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop after old-server 404")
	}
	if got := claims.Load(); got != 1 {
		t.Fatalf("claims after turn ready = %d, want 1", got)
	}
}

func TestTaskSupplementLoopAcknowledgesBeforeTurnEnds(t *testing.T) {
	var claimCount atomic.Int32
	ackSeen := make(chan struct{}, 1)
	d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/supplements/claim"):
			if claimCount.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]string{
					"comment_id": "comment-1", "author_name": "Ada", "content": "Create evidence.txt",
				})
				return
			}
			w.WriteHeader(http.StatusPreconditionFailed)
		case strings.HasSuffix(r.URL.Path, "/supplements/comment-1/ack"):
			var body struct {
				Delivered bool   `json:"delivered"`
				Error     string `json:"error"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !body.Delivered || body.Error != "" {
				t.Errorf("ack = %#v, want delivered", body)
			}
			ackSeen <- struct{}{}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	})
	injected := make(chan string, 1)
	session := &agent.Session{
		SupplementReady: func() bool { return true },
		Supplement: func(_ context.Context, instruction string) error {
			injected <- instruction
			return nil
		},
	}
	wakeup, unsubscribe := d.taskSupplementWakeup("task-ack")
	defer unsubscribe()
	d.runTaskSupplementLoop(context.Background(), context.Background(), session, "task-ack", wakeup, d.logger)
	select {
	case instruction := <-injected:
		if !strings.Contains(instruction, "Preserve and complete the original objective") || !strings.Contains(instruction, "Create evidence.txt") {
			t.Fatalf("injected instruction lost framing or content: %q", instruction)
		}
	default:
		t.Fatal("message was not injected")
	}
	select {
	case <-ackSeen:
	default:
		t.Fatal("loop returned before delivery acknowledgement")
	}
}

func TestTaskSupplementLoopUsesStableFailureReason(t *testing.T) {
	var claimCount atomic.Int32
	reasonSeen := make(chan string, 1)
	d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/supplements/claim") {
			if claimCount.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]string{"comment_id": "comment-1", "content": "Do it"})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reasonSeen <- body.Error
		w.WriteHeader(http.StatusOK)
	})
	session := &agent.Session{
		SupplementReady: func() bool { return true },
		Supplement:      func(context.Context, string) error { return context.DeadlineExceeded },
	}
	wakeup, unsubscribe := d.taskSupplementWakeup("task-failure")
	defer unsubscribe()
	d.runTaskSupplementLoop(context.Background(), context.Background(), session, "task-failure", wakeup, d.logger)
	if got := <-reasonSeen; got != protocol.TaskSupplementFailureTimeout {
		t.Fatalf("workspace-visible reason = %q, want %q", got, protocol.TaskSupplementFailureTimeout)
	}
}

type supplementGateBackend struct {
	supplementCalls atomic.Int32
}

func (b *supplementGateBackend) Execute(context.Context, string, agent.ExecOptions) (*agent.Session, error) {
	messages := make(chan agent.Message)
	close(messages)
	results := make(chan agent.Result, 1)
	results <- agent.Result{Status: "completed", Output: "done"}
	return &agent.Session{
		Messages:        messages,
		Result:          results,
		SupplementReady: func() bool { return true },
		Supplement: func(context.Context, string) error {
			b.supplementCalls.Add(1)
			return nil
		},
	}, nil
}

func TestExecuteAndDrainUnnegotiatedMakesNoSupplementRequest(t *testing.T) {
	var requests atomic.Int32
	d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	backend := &supplementGateBackend{}
	if _, _, err := d.executeAndDrain(context.Background(), backend, "original", agent.ExecOptions{}, d.logger, "task-unnegotiated", "", new(atomic.Int32), false); err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("supplement HTTP requests = %d, want 0", got)
	}
	if got := backend.supplementCalls.Load(); got != 0 {
		t.Fatalf("supplement calls = %d, want 0", got)
	}
}

func TestTaskSupplementSignalSubscriptionCleansUp(t *testing.T) {
	d := &Daemon{taskSupplementSignals: newTaskSupplementSignals()}
	_, unsubscribe := d.taskSupplementWakeup("task-cleanup")
	d.signalTaskSupplementWakeup("task-cleanup")
	unsubscribe()
	d.taskSupplementSignals.mu.Lock()
	defer d.taskSupplementSignals.mu.Unlock()
	if len(d.taskSupplementSignals.byTask) != 0 {
		t.Fatalf("signal subscriptions leaked: %#v", d.taskSupplementSignals.byTask)
	}
}
