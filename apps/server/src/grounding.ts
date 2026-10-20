// The Local Ghana Grounding Store (plan §7.5): curated, verified snippets
// matched in memory before any model call. A direct hit answers at zero
// token cost; a partial hit is injected into the prompt as a verified fact.
import { readFileSync } from "node:fs";
import { parse as parseYAML } from "yaml";
import { GSM7_SINGLE, analyze } from "./formatting";
import { resolveAsset } from "./paths";

export interface Snippet {
  slug: string;
  category: string;
  triggers: string[];
  answer: string;
  context?: string;
  direct_reply?: boolean;
  reviewed?: boolean;
  source?: string;
  enabled?: boolean;
}

export const isEnabled = (s: Snippet) => s.enabled !== false;
/** The text injected into the prompt for partial matches. */
export const factOf = (s: Snippet) => s.context || s.answer;

/** Parses and validates a YAML seed document. */
export function parseSeed(text: string): Snippet[] {
  let doc: { version?: number; snippets?: Partial<Snippet>[] };
  try {
    doc = parseYAML(text) ?? {};
  } catch (err) {
    throw new Error(`grounding: parse seed: ${(err as Error).message}`);
  }
  if (doc.version !== 1) throw new Error(`grounding: unsupported seed version ${doc.version}`);
  const errs: string[] = [];
  const seen = new Set<string>();
  const out: Snippet[] = [];
  (doc.snippets ?? []).forEach((raw, i) => {
    const s: Snippet = {
      ...raw,
      slug: raw.slug ?? "",
      category: raw.category ?? "",
      triggers: raw.triggers ?? [],
      answer: (raw.answer ?? "").trim(),
    };
    if (!s.slug) {
      errs.push(`snippet #${i + 1}: slug is required`);
      return;
    }
    if (seen.has(s.slug)) errs.push(`${s.slug}: duplicate slug`);
    seen.add(s.slug);
    if (s.triggers.length === 0) errs.push(`${s.slug}: at least one trigger is required`);
    const st = analyze(s.answer);
    if (st.encoding !== "gsm7" || st.units > GSM7_SINGLE || st.units === 0) {
      errs.push(`${s.slug}: answer must be 1-160 GSM-7 septets (got ${st.encoding}/${st.units})`);
    }
    if (!s.category) errs.push(`${s.slug}: category is required`);
    out.push(s);
  });
  if (errs.length > 0) throw new Error(errs.join("\n"));
  return out;
}

export function loadSeedFile(file: string): Snippet[] {
  return parseSeed(readFileSync(file, "utf8"));
}

/** The bundled seed: data/ in the working directory (Docker) or at the repository root (development). */
export function defaultSeedPath(): string | null {
  return resolveAsset("data/ghana_grounding_seed.yaml");
}

export interface MatchResult {
  /** Set when the snippet answer should be sent as-is. */
  direct?: Snippet;
  /** Verified facts to inject into the model prompt. */
  context: Snippet[];
}

interface Entry {
  snippet: Snippet;
  triggers: string[][];
}

// A trigger matches when at least this share of its words appear in the
// question (all words for 2-3 word triggers).
const MATCH_THRESHOLD = 0.75;
// A direct reply is skipped when the question carries more than this many
// content words beyond the trigger: it is probably asking something else.
const MAX_EXTRA_WORDS_FOR_DIRECT = 4;

/** An in-memory index over snippets. */
export class Matcher {
  private entries: Entry[] = [];

  constructor(snippets: Snippet[] = []) {
    this.replace(snippets);
  }

  /** Swaps the indexed snippets (used on reload). */
  replace(snippets: Snippet[]): void {
    const entries: Entry[] = [];
    for (const s of snippets) {
      if (!isEnabled(s)) continue;
      const triggers = s.triggers.map(tokenize).filter((t) => t.length > 0);
      if (triggers.length > 0) entries.push({ snippet: s, triggers });
    }
    this.entries = entries;
  }

  get size(): number {
    return this.entries.length;
  }

  /** Classifies a question as a direct hit, context-only, or a miss. */
  match(question: string): MatchResult {
    const q = tokenize(question);
    if (q.length === 0) return { context: [] };
    const hits: { snippet: Snippet; score: number; specificity: number; extra: number }[] = [];
    for (const e of this.entries) {
      const best = { snippet: e.snippet, score: 0, specificity: 0, extra: 0 };
      for (const trig of e.triggers) {
        let matched = 0;
        const usedQ = new Set<number>();
        for (const t of trig) {
          for (let qi = 0; qi < q.length; qi++) {
            if (!usedQ.has(qi) && tokenMatch(t, q[qi]!)) {
              matched++;
              usedQ.add(qi);
              break;
            }
          }
        }
        const score = matched / trig.length;
        if (score > best.score || (score === best.score && trig.length > best.specificity)) {
          best.score = score;
          best.specificity = trig.length;
          best.extra = q.length - usedQ.size;
        }
      }
      if (best.score >= MATCH_THRESHOLD) hits.push(best);
    }
    if (hits.length === 0) return { context: [] };
    hits.sort((a, b) => (a.score !== b.score ? b.score - a.score : b.specificity - a.specificity));
    const full = hits.filter((h) => h.score === 1).length;
    const top = hits[0]!;
    if (top.score === 1 && full === 1 && top.snippet.direct_reply && top.extra <= MAX_EXTRA_WORDS_FOR_DIRECT) {
      return { direct: top.snippet, context: [] };
    }
    return { context: hits.slice(0, 2).map((h) => h.snippet) };
  }
}

function tokenMatch(trigger: string, query: string): boolean {
  if (trigger === query) return true;
  if (trigger.length >= 4 && query.length >= 4) return query.startsWith(trigger) || trigger.startsWith(query);
  return false;
}

const stopwords = new Set(
  `a an the i me my mine we our you your do does did how what whats when where
  which who why is are was were be been am can could should would will shall may might must to of for on in
  at by with from about into and or but if then so it its this that these those there please pls plz tell
  explain know want need help give show get let us any some way ways much many im ive dont cant`.split(/\s+/),
);

/** Lowercases, strips punctuation and possessives, removes stopwords and applies a light stemmer. */
export function tokenize(s: string): string[] {
  s = s.toLowerCase().replaceAll("'s", "").replaceAll("’s", "").replaceAll("'", "").replaceAll("’", "");
  return s
    .split(/[^a-z0-9]+/)
    .filter((f) => f !== "" && !stopwords.has(f))
    .map(stem);
}

function stem(t: string): string {
  if (t.length > 5 && t.endsWith("ing")) return t.slice(0, -3);
  if (t.length > 5 && t.endsWith("al")) return t.slice(0, -2);
  if (t.length > 4 && t.endsWith("ed")) return t.slice(0, -2);
  if (t.length > 3 && t.endsWith("s") && !t.endsWith("ss") && !t.endsWith("us") && !t.endsWith("is")) return t.slice(0, -1);
  return t;
}
