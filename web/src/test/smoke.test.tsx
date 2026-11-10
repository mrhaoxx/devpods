import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithProviders } from "./render";

describe("component test harness", () => {
  it("renders into jsdom with the providers", () => {
    renderWithProviders(<button>好</button>);
    expect(screen.getByRole("button", { name: "好" })).toBeTruthy();
  });
});
