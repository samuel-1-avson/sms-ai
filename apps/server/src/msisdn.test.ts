import { describe, expect, it } from "vitest";
import { carrier, mask, normalize } from "./msisdn";

describe("normalize", () => {
  it.each([
    ["0244123456", "+233244123456"],
    ["233244123456", "+233244123456"],
    ["+233244123456", "+233244123456"],
    ["+233 24 412 3456", "+233244123456"],
    ["00233244123456", "+233244123456"],
    ["(024) 412-3456", "+233244123456"],
    ["244123456", "+233244123456"],
    ["+447700900123", "+447700900123"],
  ])("%s", (input, want) => expect(normalize(input)).toBe(want));

  it.each(["", "abc", "+23324412345", "02441234567", "12", "+0123456789", "024-41x-3456", "MTN"])("rejects %j", (bad) => {
    expect(() => normalize(bad)).toThrow();
  });
});

describe("carrier", () => {
  it.each([
    ["+233244123456", "mtn"],
    ["+233554123456", "mtn"],
    ["+233204123456", "telecel"],
    ["+233504123456", "telecel"],
    ["+233264123456", "at"],
    ["+233574123456", "at"],
    ["+233304123456", "unknown"],
    ["+447700900123", "unknown"],
  ])("%s", (input, want) => expect(carrier(input)).toBe(want));
});

it("mask", () => expect(mask("+233244123456")).toBe("+233*******56"));
