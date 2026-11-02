import { describe, expect, it } from "vitest";
import { pullTransform } from "../src/inbox-refresh.js";

describe("pullTransform", () => {
  it("applies 0.4 damping and caps the content shift at 120px", () => {
    expect(pullTransform(0)).toBe("translateY(0px)");
    expect(pullTransform(25)).toBe("translateY(10px)");
    expect(pullTransform(70)).toBe("translateY(28px)");
    expect(pullTransform(500)).toBe("translateY(120px)");
    expect(pullTransform(-10)).toBe("translateY(0px)");
  });
});
