import { describe, expect, it } from "vitest";
import { Texts, codeAcceptedText, limitReachedText, parseCommand } from "./commands";
import { analyze } from "./formatting";

describe("parseCommand", () => {
  it.each([
    ["help", { kind: "HELP" }],
    [" HELP ", { kind: "HELP" }],
    ["?", { kind: "HELP" }],
    ["more", { kind: "MORE" }],
    ["More.", { kind: "MORE" }],
    ["m", { kind: "MORE" }],
    ["NEW", { kind: "NEW" }],
    ["stop", { kind: "STOP" }],
    ["Start!", { kind: "START" }],
    ["code stemgh", { kind: "CODE", arg: "STEMGH" }],
    ["CODE: WASSCE2026", { kind: "CODE", arg: "WASSCE2026" }],
    ["delete my data", { kind: "DELETE" }],
    ["more examples of verbs", { kind: null }],
    ["What is photosynthesis?", { kind: null }],
    ["code", { kind: null }],
    ["stop the bleeding", { kind: null }],
    ["constructor", { kind: null }],
  ])("%j", (input, want) => expect(parseCommand(input)).toEqual(want));
});

it("every reply text fits one GSM-7 segment", () => {
  const texts = [
    ...Object.values(Texts),
    limitReachedText(15, 15),
    limitReachedText(150, 150),
    codeAcceptedText(100, 1000),
  ];
  for (const s of texts) {
    const st = analyze(s);
    expect({ s, enc: st.encoding, seg: st.segments }).toEqual({ s, enc: "gsm7", seg: 1 });
  }
});
