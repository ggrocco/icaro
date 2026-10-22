import type { UsageRecord } from "./api";

export interface UsageTotals {
  messages: number;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  // cost sums the priced rows only; unpriced is true when any row had no
  // price, so the real spend is at least cost.
  cost: number;
  unpriced: boolean;
}

export interface UsageDay extends UsageTotals {
  day: string;
}

const zero = (): UsageTotals => ({ messages: 0, input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, unpriced: false });

function add(t: UsageTotals, r: UsageRecord) {
  t.messages += r.messages;
  t.input += r.input_tokens;
  t.output += r.output_tokens;
  t.cacheRead += r.cache_read_tokens;
  t.cacheWrite += r.cache_write_tokens;
  if (r.cost_usd_estimate === undefined) t.unpriced = true;
  else t.cost += r.cost_usd_estimate;
}

// summarizeUsage totals the readable rows overall and per day (newest
// first); unavailable rows are returned apart so they are never summed.
export function summarizeUsage(records: UsageRecord[]) {
  const total = zero();
  const days = new Map<string, UsageDay>();
  const unavailable: UsageRecord[] = [];
  for (const r of records) {
    if (r.error || !r.day) {
      unavailable.push(r);
      continue;
    }
    add(total, r);
    let d = days.get(r.day);
    if (!d) {
      d = { day: r.day, ...zero() };
      days.set(r.day, d);
    }
    add(d, r);
  }
  return { total, days: [...days.values()].sort((a, b) => b.day.localeCompare(a.day)), unavailable };
}

const tokens = new Intl.NumberFormat();

export const fmtTokens = (n: number) => tokens.format(n);

// fmtCost labels every amount as an estimate ("~$"); amounts with
// unpriced usage behind them read "≥ ~$".
export function fmtCost(usd: number | undefined, unpriced = false) {
  if (usd === undefined) return "n/a";
  const s = `~$${usd.toFixed(usd < 1 ? 4 : 2)}`;
  return unpriced ? `≥ ${s}` : s;
}
