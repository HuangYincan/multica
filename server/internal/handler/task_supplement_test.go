package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type supplementFixture struct {
	runtimeID string
	agentID   string
	issueID   string
	taskID    string
}

func TestStableTaskSupplementFailureReason(t *testing.T) {
	for _, reason := range []string{
		protocol.TaskSupplementFailureTurnNotStarted,
		protocol.TaskSupplementFailureProviderRejected,
		protocol.TaskSupplementFailureTimeout,
		protocol.TaskSupplementFailureTurnEnded,
	} {
		if got := stableTaskSupplementFailureReason(reason); got != reason {
			t.Fatalf("stableTaskSupplementFailureReason(%q) = %q", reason, got)
		}
	}
	for _, raw := range []string{"", "供应商错误：无法发送", strings.Repeat("界", 600)} {
		if got := stableTaskSupplementFailureReason(raw); got != protocol.TaskSupplementFailureProviderRejected {
			t.Fatalf("raw reason %q escaped as %q", raw, got)
		}
	}
}

func newSupplementFixture(t *testing.T, provider, status string, negotiated bool) supplementFixture {
	t.Helper()
	runtimeID := dbfx.Runtime(t, "supplement-"+provider, testutil.Cols{"provider": provider})
	agentID := dbfx.Agent(t, "Supplement "+provider, runtimeID)
	issueID := dbfx.Issue(t, "supplement "+provider)
	triggerID := dbfx.Comment(t, issueID, "original objective")
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":           issueID,
		"runtime_id":         runtimeID,
		"trigger_comment_id": triggerID,
		"status":             status,
		"started_at":         testutil.Raw("CASE WHEN '" + status + "' = 'running' THEN now() ELSE NULL END"),
	})
	if negotiated {
		dbfx.Exec(t, `
			INSERT INTO task_supplement_capability (task_id, workspace_id, issue_id, capability)
			VALUES ($1, $2, $3, $4)
		`, taskID, testWorkspaceID, issueID, protocol.DaemonCapabilityTaskSupplementV1)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM task_supplement_capability WHERE task_id = $1`, taskID)
		})
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM task_supplement WHERE task_id = $1`, taskID)
	})
	return supplementFixture{runtimeID: runtimeID, agentID: agentID, issueID: issueID, taskID: taskID}
}

func supplementRequest(t *testing.T, fixture supplementFixture, requestID, content string) *testutil.Response {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/tasks/"+fixture.taskID+"/supplements", map[string]any{
		"client_request_id": requestID,
		"content":           content,
	})
	return testutil.Call(t, testHandler.CreateTaskSupplement,
		withURLParams(req, "id", fixture.issueID, "taskId", fixture.taskID),
	)
}

func TestTaskSupplementNegotiationFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	for _, tc := range []struct {
		name              string
		provider          string
		daemonAdvertises  bool
		wantCapabilityRow bool
	}{
		{name: "codex negotiated", provider: "codex", daemonAdvertises: true, wantCapabilityRow: true},
		{name: "old daemon", provider: "codex", daemonAdvertises: false, wantCapabilityRow: false},
		{name: "unsupported runtime", provider: "claude", daemonAdvertises: true, wantCapabilityRow: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSupplementFixture(t, tc.provider, "dispatched", false)
			started, err := testHandler.TaskService.StartTask(context.Background(), parseUUID(fixture.taskID), tc.daemonAdvertises)
			if err != nil {
				t.Fatalf("StartTask: %v", err)
			}
			if started.Status != "running" || !started.StartedAt.Valid {
				t.Fatalf("StartTask returned stale row: status=%q started=%v", started.Status, started.StartedAt.Valid)
			}
			var count int
			dbfx.QueryRow(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id = $1`, fixture.taskID).Scan(&count)
			if got := count == 1; got != tc.wantCapabilityRow {
				t.Fatalf("capability row = %v, want %v", got, tc.wantCapabilityRow)
			}
			if !tc.wantCapabilityRow {
				supplementRequest(t, fixture, "0199a4e8-22ce-7b01-bba5-111111111111", "extra").Want(http.StatusPreconditionFailed)
			}
		})
	}
}

func TestTaskSupplementCapabilityDoesNotBreakNonIssueCodexStarts(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "supplement-no-issue-codex", testutil.Cols{"provider": "codex"})
	agentID := dbfx.Agent(t, "Supplement no issue Codex", runtimeID)
	chatSessionID := dbfx.ChatSession(t, agentID)

	autopilotID := dbfx.Insert(t, "autopilot", testutil.Cols{
		"workspace_id": testWorkspaceID, "title": "supplement run only", "assignee_id": agentID,
		"execution_mode": "run_only", "created_by_type": "member", "created_by_id": testUserID,
	})
	autopilotRunID := dbfx.Insert(t, "autopilot_run", testutil.Cols{
		"autopilot_id": autopilotID, "source": "manual", "status": "running",
	})

	for _, tc := range []struct {
		name string
		cols testutil.Cols
	}{
		{name: "chat", cols: testutil.Cols{"chat_session_id": chatSessionID}},
		{name: "quick create", cols: testutil.Cols{"context": testutil.Raw(`'{"type":"quick_create","workspace_id":"` + testWorkspaceID + `","prompt":"create"}'::jsonb`)}},
		{name: "autopilot run only", cols: testutil.Cols{"autopilot_run_id": autopilotRunID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cols := testutil.Cols{"runtime_id": runtimeID, "issue_id": nil, "status": "dispatched"}
			for key, value := range tc.cols {
				cols[key] = value
			}
			taskID := dbfx.Task(t, agentID, cols)
			started, err := testHandler.TaskService.StartTask(context.Background(), parseUUID(taskID), true)
			if err != nil {
				t.Fatalf("StartTask: %v", err)
			}
			if started.Status != "running" {
				t.Fatalf("status = %q, want running", started.Status)
			}
			var capabilityRows int
			dbfx.QueryRow(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id = $1`, taskID).Scan(&capabilityRows)
			if capabilityRows != 0 {
				t.Fatalf("capability rows = %d, want 0", capabilityRows)
			}
		})
	}
}

func TestStartTaskReturnsOnlyCommittedSupplementCapability(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newSupplementFixture(t, "codex", "dispatched", false)
	req := withURLParams(newRequest(http.MethodPost, "/api/daemon/tasks/"+fixture.taskID+"/start", map[string]any{
		"capabilities": []string{protocol.DaemonCapabilityTaskSupplementV1},
	}), "taskId", fixture.taskID)
	var response AgentTaskResponse
	testutil.Call(t, testHandler.StartTask, req).Want(http.StatusOK).JSON(&response)
	if response.SupplementCapability != protocol.DaemonCapabilityTaskSupplementV1 {
		t.Fatalf("supplement_capability = %q", response.SupplementCapability)
	}
}

func TestTaskSupplementOrderedReceiptsRetryAndIdempotency(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newSupplementFixture(t, "codex", "running", true)

	var first CommentResponse
	supplementRequest(t, fixture, "0199a4e8-22ce-7b01-bba5-222222222222", "first addition").
		Want(http.StatusCreated).JSON(&first)
	if first.SupplementTaskID != fixture.taskID || first.SupplementStatus != "pending" {
		t.Fatalf("first receipt = task %q status %q", first.SupplementTaskID, first.SupplementStatus)
	}
	var duplicate CommentResponse
	supplementRequest(t, fixture, "0199a4e8-22ce-7b01-bba5-222222222222", "first addition").
		Want(http.StatusOK).JSON(&duplicate)
	if duplicate.ID != first.ID {
		t.Fatalf("duplicate request created %q, want existing %q", duplicate.ID, first.ID)
	}
	var second CommentResponse
	supplementRequest(t, fixture, "0199a4e8-22ce-7b01-bba5-333333333333", "second addition").
		Want(http.StatusCreated).JSON(&second)

	claimedFirst, err := testHandler.Queries.ClaimNextTaskSupplement(context.Background(), parseUUID(fixture.taskID))
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if uuidToString(claimedFirst.CommentID) != first.ID || claimedFirst.Content != "first addition" {
		t.Fatalf("first claim = %s %q", uuidToString(claimedFirst.CommentID), claimedFirst.Content)
	}
	delivered, err := testHandler.Queries.AckTaskSupplementDelivered(context.Background(), db.AckTaskSupplementDeliveredParams{
		TaskID: parseUUID(fixture.taskID), CommentID: claimedFirst.CommentID,
	})
	if err != nil || delivered.Status != "delivered" || !delivered.DeliveredAt.Valid {
		t.Fatalf("delivered receipt = %#v, err %v", delivered, err)
	}
	claimedSecond, err := testHandler.Queries.ClaimNextTaskSupplement(context.Background(), parseUUID(fixture.taskID))
	if err != nil || uuidToString(claimedSecond.CommentID) != second.ID {
		t.Fatalf("second claim = %#v, err %v", claimedSecond, err)
	}
	failed, err := testHandler.Queries.AckTaskSupplementFailed(context.Background(), db.AckTaskSupplementFailedParams{
		TaskID: parseUUID(fixture.taskID), CommentID: claimedSecond.CommentID,
		FailureReason: pgtype.Text{String: "provider rejected the steer", Valid: true},
	})
	if err != nil || failed.Status != "failed" || failed.FailureReason.String == "" {
		t.Fatalf("failed receipt = %#v, err %v", failed, err)
	}

	retryReq := withURLParams(newRequest(http.MethodPost, "/retry", nil),
		"id", fixture.issueID, "taskId", fixture.taskID, "commentId", second.ID)
	testutil.Call(t, testHandler.RetryTaskSupplement, retryReq).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, fixture.taskID)
	settled, err := testHandler.Queries.GetTaskSupplementByComment(context.Background(), db.GetTaskSupplementByCommentParams{
		CommentID: parseUUID(second.ID), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil || settled.Status != "failed" || settled.FailureReason.String != "turn_ended" {
		t.Fatalf("terminal settlement = %#v, err %v", settled, err)
	}
	var taskCount int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID).Scan(&taskCount)
	if taskCount != 1 {
		t.Fatalf("supplements created %d runs, want exactly one", taskCount)
	}
}

func TestTaskSupplementConcurrentCreationAllocatesOneOrderedSequence(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newSupplementFixture(t, "codex", "running", true)

	const additions = 20
	errs := make(chan error, additions)
	var wg sync.WaitGroup
	for i := 0; i < additions; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := testHandler.Queries.CreateTaskSupplement(context.Background(), db.CreateTaskSupplementParams{
				TaskID:          parseUUID(fixture.taskID),
				IssueID:         parseUUID(fixture.issueID),
				WorkspaceID:     parseUUID(testWorkspaceID),
				AuthorID:        parseUUID(testUserID),
				Content:         fmt.Sprintf("concurrent addition %02d", i),
				ClientRequestID: parseUUID(fmt.Sprintf("0199a4e8-22ce-7b01-bba5-%012x", i+1)),
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}

	var count, distinct int
	var minOrdinal, maxOrdinal int64
	dbfx.QueryRow(t, `
		SELECT count(*), count(DISTINCT ordinal), min(ordinal), max(ordinal)
		FROM task_supplement WHERE task_id = $1
	`, fixture.taskID).Scan(&count, &distinct, &minOrdinal, &maxOrdinal)
	if count != additions || distinct != additions || minOrdinal != 1 || maxOrdinal != additions {
		t.Fatalf("sequence count=%d distinct=%d range=%d..%d, want %d unique ordinals 1..%d",
			count, distinct, minOrdinal, maxOrdinal, additions, additions)
	}

	for wantOrdinal := int64(1); wantOrdinal <= additions; wantOrdinal++ {
		claimed, err := testHandler.Queries.ClaimNextTaskSupplement(context.Background(), parseUUID(fixture.taskID))
		if err != nil {
			t.Fatalf("claim ordinal %d: %v", wantOrdinal, err)
		}
		if claimed.Ordinal != wantOrdinal {
			t.Fatalf("claimed ordinal %d, want %d", claimed.Ordinal, wantOrdinal)
		}
		if _, err := testHandler.Queries.AckTaskSupplementDelivered(context.Background(), db.AckTaskSupplementDeliveredParams{
			TaskID: parseUUID(fixture.taskID), CommentID: claimed.CommentID,
		}); err != nil {
			t.Fatalf("ack ordinal %d: %v", wantOrdinal, err)
		}
	}

	var taskCount int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID).Scan(&taskCount)
	if taskCount != 1 {
		t.Fatalf("concurrent additions created %d runs, want one", taskCount)
	}
}

func TestTaskSupplementTerminalRaceCreatesNothing(t *testing.T) {
	for _, status := range []string{"completed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			assertTaskSupplementTerminalRaceCreatesNothing(t, status)
		})
	}
}

func assertTaskSupplementTerminalRaceCreatesNothing(t *testing.T, terminalStatus string) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newSupplementFixture(t, "codex", "running", true)
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT id FROM agent_task_queue WHERE id = $1 FOR UPDATE`, fixture.taskID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, createErr := testHandler.Queries.CreateTaskSupplement(context.Background(), db.CreateTaskSupplementParams{
			TaskID: parseUUID(fixture.taskID), IssueID: parseUUID(fixture.issueID), WorkspaceID: parseUUID(testWorkspaceID),
			AuthorID: parseUUID(testUserID), Content: "must not orphan",
			ClientRequestID: parseUUID("0199a4e8-22ce-7b01-bba5-444444444444"),
		})
		result <- createErr
	}()
	if _, err := tx.Exec(context.Background(), `UPDATE agent_task_queue SET status = $2, completed_at = now() WHERE id = $1`, fixture.taskID, terminalStatus); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("race create error = %v, want no rows", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("race create remained blocked")
	}
	var comments, supplements int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = 'must not orphan'`, fixture.issueID).Scan(&comments)
	dbfx.QueryRow(t, `SELECT count(*) FROM task_supplement WHERE task_id = $1`, fixture.taskID).Scan(&supplements)
	if comments != 0 || supplements != 0 {
		t.Fatalf("terminal race left comments=%d supplements=%d", comments, supplements)
	}
}

func TestTaskSupplementStopRemainsIndependent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newSupplementFixture(t, "codex", "running", true)
	var comment CommentResponse
	supplementRequest(t, fixture, "0199a4e8-22ce-7b01-bba5-777777777777", "pending when stopped").
		Want(http.StatusCreated).JSON(&comment)
	cancelReq := withURLParams(newRequest(http.MethodPost, "/cancel", nil),
		"id", fixture.issueID, "taskId", fixture.taskID)
	testutil.Call(t, testHandler.CancelTask, cancelReq).Want(http.StatusOK)
	receipt, err := testHandler.Queries.GetTaskSupplementByComment(context.Background(), db.GetTaskSupplementByCommentParams{
		CommentID: parseUUID(comment.ID), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil || receipt.Status != "failed" || receipt.FailureReason.String != "turn_ended" {
		t.Fatalf("stop receipt = %#v, err %v", receipt, err)
	}
	var taskCount int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID).Scan(&taskCount)
	if taskCount != 1 {
		t.Fatalf("stop or supplement created %d runs, want one", taskCount)
	}
}

func TestTaskSupplementPermissionAndTenantIsolation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newSupplementFixture(t, "codex", "running", true)
	otherUser := dbfx.Insert(t, "user", testutil.Cols{"name": "No Invoke", "email": "no-invoke-supplement@example.test"})
	dbfx.Member(t, testWorkspaceID, otherUser, "member")
	req := newRequestAs(otherUser, http.MethodPost, "/supplements", map[string]any{
		"client_request_id": "0199a4e8-22ce-7b01-bba5-555555555555", "content": "not allowed",
	})
	testutil.Call(t, testHandler.CreateTaskSupplement,
		withURLParams(req, "id", fixture.issueID, "taskId", fixture.taskID)).Want(http.StatusForbidden)

	foreignIssueID, foreignTaskID := setupForeignWorkspaceFixture(t)
	crossTenant := newRequest(http.MethodPost, "/api/issues/"+foreignIssueID+"/tasks/"+foreignTaskID+"/supplements", map[string]any{
		"client_request_id": "0199a4e8-22ce-7b01-bba5-666666666666", "content": "cross tenant",
	})
	testutil.Call(t, testHandler.CreateTaskSupplement,
		withURLParams(crossTenant, "id", foreignIssueID, "taskId", foreignTaskID)).Want(http.StatusNotFound)

	var count int
	dbfx.QueryRow(t, `SELECT count(*) FROM task_supplement WHERE task_id = $1`, fixture.taskID).Scan(&count)
	if count != 0 {
		t.Fatalf("denied requests created %d supplements", count)
	}
}
