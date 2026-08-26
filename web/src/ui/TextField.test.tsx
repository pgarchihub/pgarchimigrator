import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { TextField } from "./TextField";

describe("TextField accessibility", () => {
  it("associates the visible label with the input via htmlFor/id", () => {
    render(<TextField label="Email" onChange={() => {}} value="" />);
    expect(screen.getByLabelText("Email")).toBeInTheDocument();
  });

  it("adds a visual asterisk AND an accessible '(required)' cue for required fields", () => {
    render(<TextField label="Table" required onChange={() => {}} value="" />);
    // The accessible name picks up the sr-only "(required)" text too.
    const input = screen.getByLabelText(/table.*required/i);
    expect(input).toBeRequired();
  });

  it("does not add a required cue for optional fields", () => {
    render(<TextField label="Default" onChange={() => {}} value="" />);
    const input = screen.getByLabelText("Default");
    expect(input).not.toBeRequired();
    expect(screen.queryByText("*")).not.toBeInTheDocument();
  });

  it("marks the input aria-invalid and links it to the error message when an error is set", () => {
    render(<TextField label="Column" error="This field can't be empty" onChange={() => {}} value="" />);
    const input = screen.getByLabelText("Column");
    expect(input).toHaveAttribute("aria-invalid", "true");

    const describedBy = input.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy as string)).toHaveTextContent("This field can't be empty");
  });

  it("does not mark aria-invalid when there is no error", () => {
    render(<TextField label="Column" onChange={() => {}} value="" />);
    expect(screen.getByLabelText("Column")).not.toHaveAttribute("aria-invalid");
  });
});
