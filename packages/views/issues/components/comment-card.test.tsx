import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";

const { getAttachmentTextContentMock } = vi.hoisted(() => ({
  getAttachmentTextContentMock: vi.fn(),
}));

vi.mock("@multica/core/api", () => ({
  api: {
    getAttachmentTextContent: getAttachmentTextContentMock,
    getAttachment: vi.fn(),
  },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));

// HtmlAttachmentPreview (kind="html" dispatch from AttachmentBlock) reads
// useNavigation() + useWorkspaceSlug() for the Open-in-new-tab button.
// Mock both so the standalone-attachment-routes-to-iframe test does not
// need the surrounding NavigationProvider / WorkspaceSlugProvider tree.
vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/acme/issues",
    searchParams: new URLSearchParams(),
    hash: "",
    openInNewTab: vi.fn(),
    getShareableUrl: (p: string) => `https://app.example${p}`,
  }),
}));

vi.mock("@multica/core/paths", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/paths")>();
  return {
    ...actual,
    useWorkspaceSlug: () => "acme",
  };
});

import { AttachmentList, CommentDeliveryReceipts } from "./comment-card";

function renderWithQuery(ui: ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

beforeEach(() => vi.clearAllMocks());
afterEach(() => vi.restoreAllMocks());

describe("CommentDeliveryReceipts", () => {
  it("renders an independent delivery state for every agent recipient", () => {
    renderWithI18n(
      <CommentDeliveryReceipts
        entry={{
          agent_deliveries: [
            { agent_id: "a1", agent_name: "Walt", status: "pending" },
            { agent_id: "a2", agent_name: "Bob", status: "delivered", delivered_at: new Date().toISOString() },
            { agent_id: "a3", agent_name: "Kim", status: "follow_up" },
          ],
        } as any}
      />,
    );
    expect(screen.getByText("Waiting to deliver to Walt's current work")).toBeInTheDocument();
    expect(screen.getByText(/Delivered to Bob's current work/)).toBeInTheDocument();
    expect(screen.getByText("Kim will handle this in follow-up work")).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("locates only delivered recipients that already have a final reply", () => {
    const onLocateComment = vi.fn();
    const repliesByTask = new Map([
      ["task-1", { id: "reply-1", comment_type: "comment" } as any],
      ["task-2", { id: "reply-2", comment_type: "comment" } as any],
      ["task-deleted", { id: "reply-deleted", comment_type: "comment", deleted_at: "2026-01-01T00:00:00Z" } as any],
      ["task-failed", { id: "failure-comment", comment_type: "system" } as any],
    ]);
    renderWithI18n(
      <CommentDeliveryReceipts
        entry={{
          agent_deliveries: [
            { agent_id: "a1", agent_name: "Walt", task_id: "task-1", status: "delivered" },
            { agent_id: "a2", agent_name: "Bob", task_id: "task-2", status: "delivered" },
            { agent_id: "a3", agent_name: "Kim", task_id: "task-1", status: "pending" },
            { agent_id: "a4", agent_name: "Eve", task_id: "task-2", status: "follow_up" },
            { agent_id: "a5", agent_name: "Running", task_id: "task-running", status: "delivered" },
            { agent_id: "a6", agent_name: "Deleted", task_id: "task-deleted", status: "delivered" },
            { agent_id: "a7", agent_name: "Failed", task_id: "task-failed", status: "delivered" },
            { agent_id: "a8", agent_name: "Cancelled", task_id: "task-cancelled", status: "delivered" },
            { agent_id: "a9", agent_name: "Edited", task_id: "task-edited", status: "delivered" },
          ],
        } as any}
        repliesByTask={repliesByTask}
        onLocateComment={onLocateComment}
      />,
    );

    const links = screen.getAllByRole("button", { name: /View final reply/ });
    expect(links).toHaveLength(2);
    fireEvent.click(links[0]!);
    fireEvent.click(links[1]!);
    expect(onLocateComment.mock.calls).toEqual([["reply-1"], ["reply-2"]]);
    expect(screen.getByText("Waiting to deliver to Kim's current work")).toBeInTheDocument();
    expect(screen.getByText("Eve will handle this in follow-up work")).toBeInTheDocument();
    expect(screen.getByText(/Delivered to Running's current work/).closest("button")).toBeNull();
    expect(screen.getByText(/Delivered to Deleted's current work/).closest("button")).toBeNull();
    expect(screen.getByText(/Delivered to Failed's current work/).closest("button")).toBeNull();
    expect(screen.getByText(/Delivered to Cancelled's current work/).closest("button")).toBeNull();
    expect(screen.getByText(/Delivered to Edited's current work/).closest("button")).toBeNull();
  });
});

describe("AttachmentList — standalone HTML attachment routes through AttachmentBlock", () => {
  // Regression pin for comment-card.tsx:152. This is the entry point
  // MUL-2330 originally regressed on: standalone HTML attachments (not
  // referenced inline in the markdown body) MUST render through
  // <AttachmentBlock> so the html+attachmentId dispatch fires. Reverting to
  // <AttachmentCard> here re-introduces the "report.html shows as a bare
  // file card row instead of the rendered chart" bug.
  it("renders an iframe (no file-card chrome) for a standalone HTML attachment", async () => {
    getAttachmentTextContentMock.mockResolvedValueOnce({
      text: "<p>chart</p>",
      originalContentType: "text/html",
    });
    const attachment = {
      id: "att-1",
      url: "/uploads/report.html",
      filename: "report.html",
      content_type: "text/html",
      size_bytes: 0,
    } as any;

    renderWithQuery(<AttachmentList attachments={[attachment]} content="" />);

    const frame = await waitFor(() => {
      const f = document.querySelector("iframe") as HTMLIFrameElement | null;
      expect(f).toBeTruthy();
      return f!;
    });
    expect(frame.getAttribute("sandbox")).toBe("allow-scripts");
    expect(frame.getAttribute("srcdoc")).toContain("<p>chart</p>");
    // AttachmentCard chrome would render the filename as visible <p> text;
    // HtmlAttachmentPreview replaces the row entirely.
    expect(screen.queryByText("report.html")).toBeNull();
  });
});

describe("AttachmentList — inline attachment filtering", () => {
  it("does not render a bottom attachment row when the body already has the stable file-card URL", () => {
    const id = "11111111-2222-3333-4444-555555555555";
    const href = `/api/attachments/${id}/download`;
    const attachment = {
      id,
      url: "/uploads/report.pdf",
      filename: "report.pdf",
      content_type: "application/pdf",
      size_bytes: 1024,
    } as any;

    const { container } = renderWithQuery(
      <AttachmentList
        attachments={[attachment]}
        content={`!file[report.pdf](${href})`}
      />,
    );

    expect(screen.queryByText("report.pdf")).toBeNull();
    expect(container.firstChild).toBeNull();
  });

  it("does not render a bottom attachment row when the body already has the response download_url", () => {
    const href = "https://cdn.example.test/report.pdf?Signature=stale";
    const attachment = {
      id: "11111111-2222-3333-4444-555555555555",
      url: "/uploads/report.pdf",
      download_url: "https://cdn.example.test/report.pdf?Signature=fresh",
      filename: "report.pdf",
      content_type: "application/pdf",
      size_bytes: 1024,
    } as any;

    const { container } = renderWithQuery(
      <AttachmentList
        attachments={[attachment]}
        content={`!file[report.pdf](${href})`}
      />,
    );

    expect(screen.queryByText("report.pdf")).toBeNull();
    expect(container.firstChild).toBeNull();
  });
});
