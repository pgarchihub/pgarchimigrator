import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import Help from "./Help";

describe("Help", () => {
  it("shows a heading and a short product description", () => {
    render(<Help />);
    expect(screen.getByRole("heading", { name: "Help" })).toBeInTheDocument();
    expect(screen.getByText(/zero-downtime schema changes on PostgreSQL/i)).toBeInTheDocument();
  });

  it("lists product features", () => {
    render(<Help />);
    expect(screen.getByText(/8 operation types/i)).toBeInTheDocument();
    expect(screen.getByText(/Automatic strategy selection/i)).toBeInTheDocument();
  });

  // Direct regression coverage for the feature list actually being kept
  // up to date — see this file's own history: an earlier version only
  // mentioned the trust-layer basics and said nothing about Migration as
  // Code or fleet-wide analytics, both added well after the feature list
  // was first written.
  it("mentions Migration as Code and fleet-wide analytics, added after the original feature list was written", () => {
    render(<Help />);
    expect(screen.getByText(/Migration as Code/i)).toBeInTheDocument();
    expect(screen.getByText(/Fleet-wide analytics/i)).toBeInTheDocument();
  });

  it("links to the GitHub repository", () => {
    render(<Help />);
    const repoLink = screen.getByRole("link", { name: /view the source on github/i });
    expect(repoLink).toHaveAttribute("href", "https://github.com/pgarchihub/pgarchimigrator");
    // External link — must open in a new tab and not leak a referrer/
    // give the opened page access back to this window via window.opener.
    expect(repoLink).toHaveAttribute("target", "_blank");
    expect(repoLink).toHaveAttribute("rel", "noreferrer");
  });

  // Direct regression test for the actual ask: a bug/suggestion link
  // must point at GitHub's own "new issue" form specifically, not just
  // the repository's front page — someone reporting a problem shouldn't
  // have to find the Issues tab themselves.
  it("links directly to GitHub's New Issue page for bug reports and suggestions", () => {
    render(<Help />);
    const issueLink = screen.getByRole("link", { name: /report a bug or suggest a feature/i });
    expect(issueLink).toHaveAttribute("href", "https://github.com/pgarchihub/pgarchimigrator/issues/new");
    expect(issueLink).toHaveAttribute("target", "_blank");
  });

  // Direct regression test for the pgArchiHub/ArchiOrbit Labs
  // attribution replacing the earlier GitHub Sponsors link — see this
  // file's own git history for that version.
  it("links to pgArchiHub and credits ArchiOrbit Labs", () => {
    render(<Help />);
    const familyLink = screen.getByRole("link", { name: "pgArchiHub" });
    expect(familyLink).toHaveAttribute("href", "https://pgarchihub.com");
    expect(familyLink).toHaveAttribute("target", "_blank");
    expect(screen.getByText(/developed by ArchiOrbit Labs/i)).toBeInTheDocument();
  });

  it("no longer links to GitHub Sponsors", () => {
    render(<Help />);
    expect(screen.queryByRole("link", { name: /sponsor this project/i })).not.toBeInTheDocument();
  });

  // --- Changelog ---

  it("shows a v2 section with the current release's own new features", () => {
    render(<Help />);
    expect(screen.getByText("v2")).toBeInTheDocument();
    expect(screen.getByText(/12 operation types now, up from 8/i)).toBeInTheDocument();
    expect(screen.getByText(/move an entire database/i)).toBeInTheDocument();
    expect(screen.getByText(/retry a failed migration/i)).toBeInTheDocument();
  });

  it("shows a v1 section with the original release's own feature list", () => {
    render(<Help />);
    expect(screen.getByText("v1")).toBeInTheDocument();
    expect(screen.getByText(/8 operation types/i)).toBeInTheDocument();
  });

  // Direct regression test for the actual ask: from this version
  // onward, this build calls itself the Community Edition.
  it("states that this version onward is the Community Edition", () => {
    render(<Help />);
    expect(screen.getByText(/community edition/i)).toBeInTheDocument();
  });
});
