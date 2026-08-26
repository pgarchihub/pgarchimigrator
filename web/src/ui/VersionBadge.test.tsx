import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

vi.mock("../lib/api", () => ({
  api: { getVersion: vi.fn() },
}));

import { api } from "../lib/api";
import { VersionBadge } from "./VersionBadge";

describe("VersionBadge", () => {
  beforeEach(() => {
    vi.mocked(api.getVersion).mockReset();
  });

  it("shows the version once fetched", async () => {
    vi.mocked(api.getVersion).mockResolvedValue({ version: "v0.1.0" });
    render(<VersionBadge />);
    expect(await screen.findByText("v0.1.0")).toBeInTheDocument();
  });

  it("renders nothing while the fetch is still pending", () => {
    vi.mocked(api.getVersion).mockReturnValue(new Promise(() => {})); // never resolves
    const { container } = render(<VersionBadge />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing (not an error) when the fetch fails — purely cosmetic, never worth alarming over", async () => {
    vi.mocked(api.getVersion).mockRejectedValue(new Error("network down"));
    const { container } = render(<VersionBadge />);
    await waitFor(() => expect(api.getVersion).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });
});
