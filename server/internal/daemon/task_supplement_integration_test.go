//go:build agentintegration

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// TestCodexRealTaskSupplementPreservesOriginalGoal is opt-in because it starts
// three authenticated Codex turns and consumes account quota. Each sample
// injects the production framing while an explicit tool call is still active;
// any sample that drops either the original deliverable or the addition fails.
func TestCodexRealTaskSupplementPreservesOriginalGoal(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1 to allow real Codex account access")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not on PATH")
	}

	for sample := 1; sample <= 3; sample++ {
		sample := sample
		t.Run(fmt.Sprintf("sample_%d", sample), func(t *testing.T) {
			backend, err := agent.New("codex", agent.Config{
				ExecutablePath: path,
				BuiltinRuntime: true,
				Logger:         slog.Default(),
				TaskID:         fmt.Sprintf("supplement-smoke-%d", sample),
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			originalMarker := fmt.Sprintf("ORIGINAL_RETAINED_%d", sample)
			supplementMarker := fmt.Sprintf("SUPPLEMENT_RECEIVED_%d", sample)
			prompt := fmt.Sprintf("Use the shell tool to run exactly `sleep 5`. After it exits, give one concise final answer that includes the exact line %s. This line is the original task's required deliverable.", originalMarker)
			session, err := backend.Execute(ctx, prompt, agent.ExecOptions{
				Cwd:           t.TempDir(),
				Timeout:       90 * time.Second,
				ThinkingLevel: "low",
			})
			if err != nil {
				t.Fatalf("start Codex: %v", err)
			}
			if session.Supplement == nil {
				t.Fatal("Codex session did not expose supplement capability")
			}

			injected := false
			for message := range session.Messages {
				if injected || message.Type != agent.MessageToolUse {
					continue
				}
				instruction := formatTaskSupplementInstruction("smoke tester",
					fmt.Sprintf("Also include the exact line %s in the same final answer.", supplementMarker))
				injectCtx, stopInject := context.WithTimeout(ctx, 10*time.Second)
				err = session.Supplement(injectCtx, instruction)
				stopInject()
				if err != nil {
					t.Fatalf("inject supplement during tool call: %v", err)
				}
				injected = true
			}
			if !injected {
				t.Fatal("Codex never entered a tool call; supplement was not injected")
			}
			result := <-session.Result
			if result.Status != "completed" {
				t.Fatalf("Codex status=%q error=%q output=%q", result.Status, result.Error, result.Output)
			}
			if !strings.Contains(result.Output, originalMarker) || !strings.Contains(result.Output, supplementMarker) {
				t.Fatalf("final answer dropped a goal: output=%q; want %q and %q", result.Output, originalMarker, supplementMarker)
			}
			t.Logf("sample %d retained both markers in one final deliverable: %q", sample, result.Output)
		})
	}
}
