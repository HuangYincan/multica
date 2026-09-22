// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import { useTaskSupplementDraftStore } from "./task-supplement-draft-store";

describe("task supplement draft store", () => {
  beforeEach(() => useTaskSupplementDraftStore.setState({ drafts: {} }));

  it.each(["", " \n "])("clears an empty terminal draft (%j)", (content) => {
    const store = useTaskSupplementDraftStore.getState();
    store.setContent("task-1", "issue-1", content);
    store.markEnded("task-1");
    expect(useTaskSupplementDraftStore.getState().drafts["task-1"]).toBeUndefined();
  });

  it("clears a previously ended draft after its text is erased", () => {
    const store = useTaskSupplementDraftStore.getState();
    store.setContent("task-1", "issue-1", "retained");
    store.markEnded("task-1");
    store.setContent("task-1", "issue-1", "");
    store.markEnded("task-1");
    expect(useTaskSupplementDraftStore.getState().drafts["task-1"]).toBeUndefined();
  });

  it("keeps text, request identity, and terminal recovery state by task id", () => {
    const store = useTaskSupplementDraftStore.getState();
    store.open("task-1", "issue-1");
    store.setContent("task-1", "issue-1", "preserve both deliverables");
    store.setRequestId("task-1", "request-1");
    store.markEnded("task-1");

    expect(useTaskSupplementDraftStore.getState().drafts["task-1"]).toMatchObject({
      issueId: "issue-1",
      content: "preserve both deliverables",
      clientRequestId: "request-1",
      open: true,
      ended: true,
    });
  });

  it("does not let another task consume or clear the draft", () => {
    const store = useTaskSupplementDraftStore.getState();
    store.setContent("task-1", "issue-1", "first");
    store.setContent("task-2", "issue-1", "second");
    store.clear("task-2");
    expect(useTaskSupplementDraftStore.getState().drafts["task-1"]?.content).toBe("first");
  });

  it("changes request identity only when the user changes text", () => {
    const store = useTaskSupplementDraftStore.getState();
    store.setContent("task-1", "issue-1", "same text");
    store.setRequestId("task-1", "request-1");
    store.markEnded("task-1");
    expect(useTaskSupplementDraftStore.getState().drafts["task-1"]?.clientRequestId).toBe("request-1");
    store.setContent("task-1", "issue-1", "edited text");
    expect(useTaskSupplementDraftStore.getState().drafts["task-1"]?.clientRequestId).toBeUndefined();
  });
});
