import { describe, expect, it } from "vitest";
import {
  SMS_LIMITS,
  SMS_MORE_SUFFIX,
  analyze,
  containsLocalLanguageLetters,
  fits,
  isGSM7Text,
  measure,
  paginate,
  sanitize,
  segmentCount,
  splitFirst,
  truncate,
} from "./index";

const rep = (s: string, n: number) => s.repeat(n);

describe("measure", () => {
  it.each([
    ["Hello", "gsm7", 5],
    ["[a]", "gsm7", 5], // [ and ] are extension characters: 2 septets each
    ["€5", "gsm7", 3],
    ["@£$¥", "gsm7", 4],
    ["It’s", "ucs2", 4], // curly apostrophe forces UCS-2
    ["😀", "ucs2", 2], // surrogate pair counts as two UTF-16 units
  ])("%s", (input, enc, units) => {
    expect(measure(input)).toEqual([enc, units]);
  });
});

describe("segmentCount", () => {
  it.each([
    ["", 0],
    [rep("a", 160), 1],
    [rep("a", 161), 2],
    [rep("a", 306), 2],
    [rep("a", 307), 3],
    [rep("ɛ", 70), 1],
    [rep("ɛ", 71), 2],
    [rep("ɛ", 134), 2],
    [rep("ɛ", 135), 3],
    // 152 septets + € (2 septets) cannot share part one: the escape pair moves to part two.
    [rep("a", 152) + "€" + rep("a", 10), 2],
    [rep("a", 159) + "€", 2],
  ])("len %#", (input, want) => {
    expect(segmentCount(input)).toBe(want);
  });
});

describe("sanitize", () => {
  it("defuses the homoglyph trap", () => {
    const raw =
      "Photosynthesis is how plants make food from sunlight, water and carbon dioxide. It’s the reason we have oxygen in the air that we all breathe.";
    expect(segmentCount(raw)).toBe(3);
    const st = analyze(sanitize(raw));
    expect(st.encoding).toBe("gsm7");
    expect(st.segments).toBe(1);
  });

  it.each([
    ["It’s “great” — really…", `It's "great" - really...`],
    ["2 × 3 ÷ 6 ≤ 1 ≥ 0", "2 x 3 / 6 <= 1 >= 0"],
    ["H₂O and x² at 25°C", "H2O and x^2 at 25 deg C"],
    ["Earth's tilt (23.5°) causes seasons", "Earth's tilt (23.5 deg) causes seasons"],
    ["• one\n• two", "one; two"],
    ["It costs GH¢5 or ₵5", "It costs GHS5 or GHS5"],
    ["Great job 👍!", "Great job!"],
    ["Café naïve façade", "Café naive facade"],
    ["a b​c", "a bc"],
    ["Use `*929#` now", "Use *929# now"],
    ["π r²", "pi r^2"],
    ["Visit [NHIA](https://www.nhis.gov.gh) now", "Visit NHIA (https://www.nhis.gov.gh) now"],
  ])("maps %j", (input, want) => {
    const got = sanitize(input);
    expect(got).toBe(want);
    expect(isGSM7Text(got)).toBe(true);
  });

  it.each([
    ["Sure! Here is the answer: photosynthesis makes food.", "Photosynthesis makes food."],
    ["Certainly. Here’s a short explanation. Ohm’s law is V=IR.", "Ohm's law is V=IR."],
    ["## Ohm's law\n**V = I x R** (voltage = current x resistance)", "Ohm's law V = I x R (voltage = current x resistance)"],
    ["Answer: 42", "42"],
    ["Steps:\n1. Mix flour\n2. Bake it", "Steps: Mix flour; Bake it"],
    ["Reply STOP to opt out", "Reply STOP to opt out"],
    ["OK is short for okay.", "OK is short for okay."],
    ["2*3*4 = 24", "2*3*4 = 24"],
    ["Line one\n\n---\n\nLine two", "Line one Line two"],
  ])("strips filler and markdown %j", (input, want) => {
    expect(sanitize(input)).toBe(want);
  });

  it("handles local-language letters", () => {
    const input = "Ɛte sɛn? Me ho yɛ.";
    expect(sanitize(input)).toBe("Ete sen? Me ho ye.");
    const kept = sanitize(input, { allowUCS2: true });
    expect(kept).toBe(input);
    expect(measure(kept)[0]).toBe("ucs2");
    expect(containsLocalLanguageLetters(input)).toBe(true);
    expect(containsLocalLanguageLetters("hello")).toBe(false);
    // Curly quotes are still mapped even when UCS-2 is allowed.
    expect(sanitize("ɔ’s", { allowUCS2: true })).toBe("ɔ's");
  });
});

const answer240 =
  "Photosynthesis is the process plants use to turn sunlight, water and carbon dioxide into glucose and oxygen. " +
  "It happens in the chloroplasts of leaf cells. Chlorophyll absorbs the light energy that powers the reaction for growth.";

describe("splitFirst", () => {
  it("builds the MORE buffer at a sentence boundary without losing text", () => {
    expect(answer240.length).toBeGreaterThanOrEqual(220);
    expect(answer240.length).toBeLessThanOrEqual(260);
    const [first, rest] = splitFirst(answer240, SMS_LIMITS, SMS_MORE_SUFFIX);
    expect(first.endsWith("..Txt MORE")).toBe(true);
    expect(measure(first)[1]).toBeLessThanOrEqual(160);
    expect(segmentCount(first)).toBe(1);
    const head = first.slice(0, -SMS_MORE_SUFFIX.length);
    expect(head.endsWith(".")).toBe(true);
    expect(rest).not.toBe("");
    expect(fits(rest, SMS_LIMITS)).toBe(true);
    expect(head + " " + rest).toBe(answer240);
  });

  it("does not split short text", () => {
    expect(splitFirst("Ohm's law: V = I x R.", SMS_LIMITS, SMS_MORE_SUFFIX)).toEqual(["Ohm's law: V = I x R.", ""]);
  });

  it("drops dangling punctuation at a word boundary", () => {
    const [first, rest] = splitFirst(rep("plants, ", 30), SMS_LIMITS, SMS_MORE_SUFFIX);
    const head = first.slice(0, -SMS_MORE_SUFFIX.length);
    expect(head.endsWith(",")).toBe(false);
    expect(rest).not.toBe("");
  });

  it("hard-cuts text without spaces and loses nothing", () => {
    const [first, rest] = splitFirst(rep("x", 400), SMS_LIMITS, SMS_MORE_SUFFIX);
    expect(measure(first)[1]).toBeLessThanOrEqual(160);
    expect(rest.length).toBe(400 - (first.length - SMS_MORE_SUFFIX.length));
  });

  it("paginates UCS-2 text into 70-unit pages", () => {
    const [first, rest] = splitFirst(rep("Ɛyɛ adeɛ pa. ", 12), SMS_LIMITS, SMS_MORE_SUFFIX);
    const [enc, n] = measure(first);
    expect(enc).toBe("ucs2");
    expect(n).toBeLessThanOrEqual(70);
    expect(rest).not.toBe("");
  });
});

describe("truncate", () => {
  it("ends on a word with ... or on a full sentence", () => {
    const got = truncate(rep("word ", 60), SMS_LIMITS);
    expect(measure(got)[1]).toBeLessThanOrEqual(160);
    expect(got.endsWith("...")).toBe(true);
    const sentences =
      "First sentence is here and it is long enough to matter for the cut. Second sentence keeps going. " +
      "Third sentence is far too long to fit on this page at all, so it has to go.";
    expect(truncate(sentences, SMS_LIMITS)).toBe(
      "First sentence is here and it is long enough to matter for the cut. Second sentence keeps going.",
    );
  });
});

describe("paginate", () => {
  it("makes single-segment pages with suffixes on all but the last", () => {
    const pages = paginate(rep("Plants need light to grow well. ", 20), SMS_LIMITS, SMS_MORE_SUFFIX, 3);
    expect(pages).toHaveLength(3);
    pages.forEach((p, i) => {
      expect(segmentCount(p)).toBe(1);
      expect(p.endsWith(SMS_MORE_SUFFIX)).toBe(i < 2);
    });
  });
});
