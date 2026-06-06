/**
 * Unit tests for the labelled form primitives Input and Select.
 *
 * Both share `Field.module.css` and the same accessibility wiring:
 * the visible label is associated to the control via `htmlFor`/`id`,
 * hint and error text are linked through `aria-describedby`, and an
 * `error` sets `aria-invalid` and replaces the hint. Tests query by
 * the accessible label/role so they survive a styling migration.
 */
import { createRef } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { Input } from "./Input";
import { Select } from "./Select";

describe("Input", () => {
  it("associates the visible label with the control", () => {
    render(<Input label="Email address" />);
    const input = screen.getByLabelText("Email address");
    expect(input.tagName).toBe("INPUT");
  });

  it("links hint text via aria-describedby", () => {
    render(<Input label="Email" hint="We'll never share it" />);
    const input = screen.getByLabelText("Email");
    const describedBy = input.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(screen.getByText("We'll never share it")).toHaveAttribute(
      "id",
      describedBy as string,
    );
    expect(input).not.toHaveAttribute("aria-invalid");
  });

  it("marks the field invalid and shows the error instead of the hint", () => {
    render(<Input label="Email" hint="optional" error="Required field" />);
    const input = screen.getByLabelText("Email");
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Required field")).toBeInTheDocument();
    expect(screen.queryByText("optional")).not.toBeInTheDocument();
  });

  it("accepts typed input and forwards a ref", async () => {
    const ref = createRef<HTMLInputElement>();
    render(<Input label="Name" ref={ref} />);
    const input = screen.getByLabelText("Name");
    await userEvent.type(input, "Ada");
    expect(input).toHaveValue("Ada");
    expect(ref.current).toBe(input);
  });
});

describe("Select", () => {
  it("renders options from the options prop and associates the label", async () => {
    render(
      <Select
        label="Plan"
        options={[
          { value: "free", label: "Free" },
          { value: "pro", label: "Pro" },
        ]}
      />,
    );
    const select = screen.getByLabelText("Plan");
    expect(select.tagName).toBe("SELECT");
    await userEvent.selectOptions(select, "pro");
    expect(select).toHaveValue("pro");
  });

  it("sets aria-invalid and surfaces the error message", () => {
    render(
      <Select
        label="Plan"
        error="Pick a plan"
        options={[{ value: "free", label: "Free" }]}
      />,
    );
    const select = screen.getByLabelText("Plan");
    expect(select).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Pick a plan")).toBeInTheDocument();
  });

  it("supports disabled options", () => {
    render(
      <Select
        label="Plan"
        options={[
          { value: "free", label: "Free" },
          { value: "ent", label: "Enterprise", disabled: true },
        ]}
      />,
    );
    expect(
      screen.getByRole("option", { name: "Enterprise" }),
    ).toBeDisabled();
  });
});
