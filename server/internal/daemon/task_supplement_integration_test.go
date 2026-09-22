//go:build agentintegration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// TestCodexRealTaskSupplementPreservesOriginalGoal is opt-in because it starts
// three authenticated Codex turns and consumes account quota. Each sample runs
// through the production daemon claim -> steer -> acknowledgement loop and
// requires separate, inspectable artifacts for the original and injected work,
// plus one final response that reports both markers.
func TestCodexRealTaskSupplementPreservesOriginalGoal(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1 to allow real Codex account access")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not on PATH")
	}
	version, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("codex --version: %v: %s", err, version)
	}
	t.Logf("tested Codex build: %s", strings.TrimSpace(string(version)))

	for sample := 1; sample <= 3; sample++ {
		sample := sample
		t.Run(fmt.Sprintf("sample_%d", sample), func(t *testing.T) {
			workDir := t.TempDir()
			originalMarker := fmt.Sprintf("ORIGINAL_RETAINED_%d", sample)
			supplementMarker := fmt.Sprintf("SUPPLEMENT_RECEIVED_%d", sample)
			originalFile := fmt.Sprintf("original-%d.txt", sample)
			supplementFile := fmt.Sprintf("injected-%d.txt", sample)
			taskID := fmt.Sprintf("supplement-smoke-%d", sample)

			backend, err := agent.New("codex", agent.Config{
				ExecutablePath: path,
				BuiltinRuntime: true,
				Logger:         slog.Default(),
				TaskID:         taskID,
			})
			if err != nil {
				t.Fatal(err)
			}

			var claims atomic.Int32
			acknowledged := make(chan bool, 1)
			d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/supplements/claim"):
					if claims.Add(1) == 1 {
						_ = json.NewEncoder(w).Encode(map[string]string{
							"comment_id":  fmt.Sprintf("smoke-comment-%d", sample),
							"author_name": "smoke tester",
							"content": fmt.Sprintf(
								"Create %s containing exactly %s. Print the exact line %s in the final response.",
								supplementFile, supplementMarker, supplementMarker),
						})
						return
					}
					w.WriteHeader(http.StatusPreconditionFailed)
				case strings.Contains(r.URL.Path, "/supplements/") && strings.HasSuffix(r.URL.Path, "/ack"):
					var body struct {
						Delivered bool   `json:"delivered"`
						Error     string `json:"error"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode acknowledgement: %v", err)
					}
					acknowledged <- body.Delivered && body.Error == ""
					w.WriteHeader(http.StatusOK)
				default:
					// executeAndDrain also reports ordinary stream messages and the
					// session pointer; those writes are irrelevant to this isolated
					// supplement transport probe.
					w.WriteHeader(http.StatusOK)
				}
			})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			prompt := fmt.Sprintf(
				"Run `sleep 5` with the shell. After it exits, create %s containing exactly %s. Print the exact line %s in the final response.",
				originalFile, originalMarker, originalMarker)
			result, _, err := d.executeAndDrain(ctx, backend, prompt, agent.ExecOptions{
				Cwd:           workDir,
				Timeout:       90 * time.Second,
				ThinkingLevel: "low",
			}, d.logger, taskID, "", new(atomic.Int32), true)
			if err != nil {
				t.Fatalf("execute Codex: %v", err)
			}
			if result.Status != "completed" {
				t.Fatalf("Codex status=%q error=%q output=%q", result.Status, result.Error, result.Output)
			}
			select {
			case delivered := <-acknowledged:
				if !delivered {
					t.Fatal("daemon acknowledgement did not record delivery")
				}
			default:
				t.Fatal("daemon returned before delivery acknowledgement")
			}
			for path, marker := range map[string]string{
				filepath.Join(workDir, originalFile):   originalMarker,
				filepath.Join(workDir, supplementFile): supplementMarker,
			} {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read independent artifact %s: %v", filepath.Base(path), err)
				}
				if strings.TrimSpace(string(content)) != marker {
					t.Fatalf("artifact %s = %q, want %q", filepath.Base(path), content, marker)
				}
			}
			if !strings.Contains(result.Output, originalMarker) || !strings.Contains(result.Output, supplementMarker) {
				t.Fatalf("unique final answer dropped a goal: output=%q; want %q and %q", result.Output, originalMarker, supplementMarker)
			}
			t.Logf("sample %d claim_count=%d ack=delivered artifacts=%s,%s final=%q",
				sample, claims.Load(), originalFile, supplementFile, result.Output)
		})
	}
}
