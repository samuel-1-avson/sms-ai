import { describe, expect, it } from "vitest";
import { Matcher, defaultSeedPath, loadSeedFile, parseSeed, tokenize } from "./grounding";

function seedMatcher() {
  const p = defaultSeedPath();
  expect(p).not.toBeNull();
  return new Matcher(loadSeedFile(p!));
}

describe("grounding", () => {
  it("bundled seed is valid", () => {
    expect(seedMatcher().size).toBeGreaterThanOrEqual(10);
  });

  it.each([
    ["How do I check my NHIS renewal?", "nhis-check-renew"],
    ["How to link Ghana Card to MTN?", "ghana-card-sim-link"],
    ["Explain Ohm's law", "ohms-law"],
    ["explain newtons first law", "newtons-first-law"],
    ["what are the WASSCE grades", "wassce-grading"],
    ["How do I calculate my BECE aggregate", "bece-aggregate"],
    ["Should I share my MoMo PIN?", "momo-pin-safety"],
  ])("direct hit %j", (q, slug) => {
    expect(seedMatcher().match(q).direct?.slug).toBe(slug);
  });

  it.each(["What is photosynthesis?", "Solve 2x + 5 = 15", "Who won the 2010 world cup", "newton second law"])("misses %j", (q) => {
    const r = seedMatcher().match(q);
    expect(r.direct).toBeUndefined();
    expect(r.context).toHaveLength(0);
  });

  it("injects context for ambiguous or long questions", () => {
    const m = seedMatcher();
    let r = m.match("What is the difference between Ohm's law and Newton's first law?");
    expect(r.direct).toBeUndefined();
    expect(r.context).toHaveLength(2);
    r = m.match("My NHIS status shows expired but I paid last month at the office in Kumasi, what went wrong");
    expect(r.direct).toBeUndefined();
    expect(r.context[0]?.slug).toBe("nhis-check-renew");
  });

  it("rejects bad seed entries", () => {
    const bad = `version: 1
snippets:
  - slug: curly
    category: x
    triggers: ["a b"]
    answer: "It’s curly"
  - slug: long
    category: x
    triggers: ["a b"]
    answer: "${"a".repeat(161)}"
  - slug: curly
    category: x
    triggers: []
    answer: "ok"
`;
    expect(() => parseSeed(bad)).toThrow();
    try {
      parseSeed(bad);
    } catch (err) {
      for (const want of ["curly: answer must be", "long: answer must be", "duplicate slug", "at least one trigger"]) {
        expect((err as Error).message).toContain(want);
      }
    }
  });

  it("tokenizes", () => {
    expect(tokenize("How do I check my NHIS renewal?").join(" ")).toBe("check nhis renew");
  });
});
