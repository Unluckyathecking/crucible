"use client";

import React, { useState, useEffect, useCallback, useRef, useMemo } from "react";
import {
  validateDateRange,
  parseDateParam,
  toISODateString,
  bucketByDay,
  aggregateByOperation,
  MAX_USAGE_RANGE_DAYS,
  MS_PER_DAY,
  type DayBucket,
  type OperationRow,
  type RawEvent,
} from "@/lib/usage-format";
import { fetchUsage } from "@/lib/usage-fetch";
import { UsageChart } from "./usage-chart";

// Matches USAGE_WINDOW_DAYS in app/dashboard/page.tsx (30 days).
// Kept as a local constant to avoid bundling server-only code on the client.
const DEFAULT_USAGE_WINDOW_DAYS = 30;

function utcTodayStr(): string {
  const d = new Date();
  return toISODateString(
    new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate())),
  );
}

function initRange(): { from: string; to: string; apiTo: string } {
  const today = new Date();
  const todayUTC = new Date(
    Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate()),
  );
  // Exclusive upper bound. Mirrors page.tsx: thirtyDaysAgo = tomorrowMidnight - N * MS_PER_DAY.
  // [tomorrowUTC - N*MS_PER_DAY, tomorrowUTC) = N full days; display range [from, today] = N dates.
  const tomorrowUTC = new Date(todayUTC.getTime() + MS_PER_DAY);
  return {
    from: toISODateString(
      new Date(tomorrowUTC.getTime() - DEFAULT_USAGE_WINDOW_DAYS * MS_PER_DAY),
    ),
    to: toISODateString(todayUTC),
    apiTo: toISODateString(tomorrowUTC),
  };
}

type DataState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ok"; ops: OperationRow[]; buckets: DayBucket[] };

type DrillState =
  | { status: "none" }
  | { status: "loading"; operation: string }
  | { status: "error"; operation: string; message: string }
  | { status: "ok"; operation: string; events: RawEvent[] };

export function UsageClient() {
  // useState initializer ensures initRange() is called exactly once,
  // avoiding midnight-crossing inconsistency between renders and effects.
  const [init] = useState(initRange);
  const [displayFrom, setDisplayFrom] = useState(init.from);
  const [displayTo, setDisplayTo] = useState(init.to);
  const [rangeError, setRangeError] = useState<string | null>(null);
  // Track the API params used for the active query so drill-down is consistent.
  const [queryFrom, setQueryFrom] = useState(init.from);
  const [queryTo, setQueryTo] = useState(init.apiTo);
  const [data, setData] = useState<DataState>({ status: "idle" });
  const [drill, setDrill] = useState<DrillState>({ status: "none" });
  // Refs mirror state values at render time so handleDrillDown reads the
  // latest values even if called within the same event-loop tick as state updates.
  const drillRef = useRef<DrillState>(drill);
  drillRef.current = drill;
  const queryFromRef = useRef(queryFrom);
  queryFromRef.current = queryFrom;
  const queryToRef = useRef(queryTo);
  queryToRef.current = queryTo;

  const abortRef = useRef<AbortController | null>(null);
  const drillAbortRef = useRef<AbortController | null>(null);
  const drillSeqRef = useRef(0);
  // Monotonic counter so a stale in-flight loadMain response is discarded even if
  // the AbortSignal fires after fetch() has already returned (browser race window).
  const mainSeqRef = useRef(0);

  const loadMain = useCallback(async (apiFrom: string, apiTo: string, signal?: AbortSignal) => {
    setData({ status: "loading" });
    setDrill({ status: "none" });
    // Increment AFTER synchronous state updates so the seq captures the post-update epoch.
    const seq = ++mainSeqRef.current;
    try {
      const result = await fetchUsage(apiFrom, apiTo, undefined, signal);
      if (signal?.aborted) return;
      if (mainSeqRef.current !== seq) return;
      if (result === null) return;
      if ("error" in result) {
        setData({ status: "error", message: result.error });
        return;
      }
      setData({
        status: "ok",
        ops: aggregateByOperation(result.data),
        buckets: bucketByDay(result.data),
      });
    } catch {
      if (signal?.aborted) return;
      if (mainSeqRef.current === seq) {
        setData({ status: "error", message: "Failed to load usage data" });
      }
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    void loadMain(init.from, init.apiTo, ctrl.signal);
    return () => {
      abortRef.current?.abort();
      drillAbortRef.current?.abort();
    };
    // Intentionally run once on mount; loadMain is stable (useCallback with no deps).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function handleApply() {
    setDrill({ status: "none" });
    // Abort any in-flight drill-down and invalidate its sequence so a stale response
    // cannot overwrite the "none" state set above with events from the old date range.
    drillAbortRef.current?.abort();
    drillSeqRef.current++;
    const fromDate = parseDateParam(displayFrom);
    const toDate = parseDateParam(displayTo);
    if (isNaN(fromDate.getTime()) || isNaN(toDate.getTime())) {
      setRangeError("Invalid date format");
      return;
    }
    const apiFrom = toISODateString(fromDate);
    // Use the pre-validated toDate object to compute the exclusive upper bound.
    const apiTo = toISODateString(new Date(toDate.getTime() + MS_PER_DAY));
    // Validate with the exclusive upper bound to match the API contract;
    // the user-visible maximum is MAX_USAGE_RANGE_DAYS inclusive days.
    const check = validateDateRange(fromDate, new Date(toDate.getTime() + MS_PER_DAY));
    if (!check.valid) {
      setRangeError(check.error ?? "Invalid date range");
      return;
    }
    setRangeError(null);
    setQueryFrom(apiFrom);
    setQueryTo(apiTo);
    abortRef.current?.abort();
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    void loadMain(apiFrom, apiTo, ctrl.signal);
  }

  async function handleDrillDown(operation: string) {
    const latestDrill = drillRef.current;
    if ((latestDrill.status === "ok" || latestDrill.status === "loading") && latestDrill.operation === operation) {
      setDrill({ status: "none" });
      drillAbortRef.current?.abort();
      return;
    }
    drillAbortRef.current?.abort();
    const ctrl = new AbortController();
    drillAbortRef.current = ctrl;
    // Increment sequence so any concurrent in-flight response for a prior seq is discarded.
    const seq = ++drillSeqRef.current;
    setDrill({ status: "loading", operation });
    try {
      const result = await fetchUsage(queryFromRef.current, queryToRef.current, operation, ctrl.signal);
      if (ctrl.signal.aborted) return;
      if (drillSeqRef.current !== seq) return;
      if (result === null) return;
      if ("error" in result) {
        setDrill({ status: "error", operation, message: result.error });
        return;
      }
      setDrill({ status: "ok", operation, events: result.data });
    } catch {
      if (ctrl.signal.aborted) return;
      if (drillSeqRef.current !== seq) return;
      setDrill({ status: "error", operation, message: "Failed to load events" });
    }
  }

  const todayStr = utcTodayStr();

  // Memoized so parseDateParam isn't re-invoked on every render just for the
  // min/max attributes; deps are the two display values and todayStr.
  const fromMax = useMemo(() => {
    const fd = parseDateParam(displayFrom);
    const td = parseDateParam(displayTo);
    return !isNaN(fd.getTime()) && !isNaN(td.getTime()) && fd.getTime() <= td.getTime()
      ? displayTo : todayStr;
  }, [displayFrom, displayTo, todayStr]);

  const toMin = useMemo(() => {
    const fd = parseDateParam(displayFrom);
    const td = parseDateParam(displayTo);
    return !isNaN(fd.getTime()) && !isNaN(td.getTime()) && fd.getTime() <= td.getTime()
      ? displayFrom : "1970-01-01";
  }, [displayFrom, displayTo]);

  // BigInt accumulation matches the pattern in app/dashboard/page.tsx and avoids
  // IEEE 754 precision loss before the MAX_SAFE_INTEGER overflow check.
  const MAX_SAFE_BI = BigInt(Number.MAX_SAFE_INTEGER);
  // Math.trunc guards against fractional billable_units (JSON numbers allow floats;
  // BigInt() throws RangeError on non-integers).
  const totalUnitsBig =
    data.status === "ok"
      ? data.ops.reduce(
          (a, r) => a + BigInt(Math.trunc(r.total_billable_units)),
          0n,
        )
      : 0n;
  const totalCallsBig =
    data.status === "ok"
      ? data.ops.reduce((a, r) => a + BigInt(Math.trunc(r.event_count)), 0n)
      : 0n;
  const totalUnitsDisplay =
    totalUnitsBig > MAX_SAFE_BI ? "∞" : Number(totalUnitsBig).toLocaleString();
  const totalCallsDisplay =
    totalCallsBig > MAX_SAFE_BI ? "∞" : Number(totalCallsBig).toLocaleString();

  return (
    <div className="space-y-5">
      <section className="border border-zinc-200 rounded-lg p-4 sm:p-5">
        <h2 className="text-base font-semibold mb-3">Date range</h2>
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-zinc-500">From</span>
            <input
              type="date"
              value={displayFrom}
              min="1970-01-01"
              max={fromMax}
              onChange={(e) => {
                setDisplayFrom(e.target.value);
                setRangeError(null);
              }}
              className="border border-zinc-200 rounded px-2 py-1 text-sm bg-white"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-zinc-500">To (inclusive)</span>
            <input
              type="date"
              value={displayTo}
              min={toMin}
              max={todayStr}
              onChange={(e) => {
                setDisplayTo(e.target.value);
                setRangeError(null);
              }}
              className="border border-zinc-200 rounded px-2 py-1 text-sm bg-white"
            />
          </label>
          <button
            onClick={handleApply}
            disabled={data.status === "loading" || drill.status === "loading"}
            className="px-3 py-1.5 bg-zinc-900 text-white text-sm rounded hover:bg-zinc-700 disabled:opacity-50"
          >
            {data.status === "loading" ? "Loading…" : "Apply"}
          </button>
        </div>
        {rangeError && (
          <p className="mt-2 text-sm text-red-600" role="alert">
            {rangeError}
          </p>
        )}
        <p className="mt-2 text-xs text-zinc-400">Max {MAX_USAGE_RANGE_DAYS} days</p>
      </section>

      {data.status === "error" && (
        <p className="text-sm text-red-600" role="alert">
          {data.message}
        </p>
      )}

      {data.status === "ok" && (
        <>
          <section className="border border-zinc-200 rounded-lg p-4 sm:p-5">
            <h2 className="text-base font-semibold mb-3">Units over time</h2>
            <UsageChart buckets={data.buckets} />
          </section>

          <section
            className="border border-zinc-200 rounded-lg p-4 sm:p-5"
            aria-label="Usage by operation"
          >
            <h2 className="text-base font-semibold mb-3">By operation</h2>
            {data.ops.length === 0 ? (
              <p className="text-sm text-zinc-500">No usage in this period.</p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="text-left text-zinc-500 border-b border-zinc-200">
                      <th className="pb-2 pr-4 font-medium">Operation</th>
                      <th className="pb-2 pr-4 font-medium text-right">Units</th>
                      <th className="pb-2 pr-4 font-medium text-right">Calls</th>
                      <th className="pb-2 font-medium" />
                    </tr>
                  </thead>
                  <tbody>
                    {data.ops.map((row) => {
                      const isOpen =
                        drill.status === "ok" && drill.operation === row.operation;
                      const isLoadingDrill =
                        drill.status === "loading" && drill.operation === row.operation;
                      const hasError =
                        drill.status === "error" && drill.operation === row.operation;
                      return (
                        <React.Fragment key={row.operation}>
                          <tr className="border-b border-zinc-100">
                            <td className="py-2 pr-4 font-mono">{row.operation}</td>
                            <td className="py-2 pr-4 text-right tabular-nums">
                              {row.total_billable_units.toLocaleString()}
                            </td>
                            <td className="py-2 pr-4 text-right tabular-nums text-zinc-500">
                              {row.event_count.toLocaleString()}
                            </td>
                            <td className="py-2 text-right">
                              <button
                                onClick={() => void handleDrillDown(row.operation)}
                                disabled={isLoadingDrill}
                                aria-expanded={isOpen}
                                aria-label={`${isOpen ? "Hide" : "Show"} events for ${row.operation}`}
                                className="text-xs text-zinc-500 hover:text-zinc-900 underline disabled:opacity-50"
                              >
                                {isLoadingDrill ? "Loading…" : isOpen ? "Hide" : "Details"}
                              </button>
                            </td>
                          </tr>
                          {(isOpen || hasError) && (
                            <tr className="bg-zinc-50">
                              <td colSpan={4} className="px-2 py-3">
                                {hasError && drill.status === "error" && (
                                  <p className="text-sm text-red-600">{drill.message}</p>
                                )}
                                {isOpen && drill.status === "ok" && (
                                  drill.events.length === 0 ? (
                                    <p className="text-sm text-zinc-500">No events found.</p>
                                  ) : (
                                    <div className="overflow-x-auto max-h-64 overflow-y-auto">
                                      <table className="w-full text-xs">
                                        <thead className="sticky top-0 bg-zinc-50">
                                          <tr className="text-left text-zinc-400 border-b border-zinc-200">
                                            <th className="pb-1 pr-4 font-medium">
                                              Timestamp (UTC)
                                            </th>
                                            <th className="pb-1 font-medium text-right">
                                              Units
                                            </th>
                                          </tr>
                                        </thead>
                                        <tbody>
                                          {drill.events.map((e, i) => {
                                            const ts = new Date(e.created_at);
                                            return (
                                              <tr
                                                key={`${e.created_at}-${e.billable_units}-${e.operation}-${i}`}
                                                className="border-b border-zinc-100"
                                              >
                                                <td className="py-1 pr-4 font-mono">
                                                  {isNaN(ts.getTime()) ? e.created_at : ts.toISOString()}
                                                </td>
                                                <td className="py-1 text-right tabular-nums">
                                                  {(e.billable_units ?? 0).toLocaleString()}
                                                </td>
                                              </tr>
                                            );
                                          })}
                                        </tbody>
                                      </table>
                                    </div>
                                  )
                                )}
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </tbody>
                  <tfoot>
                    <tr className="text-zinc-600 font-medium border-t border-zinc-200">
                      <td className="pt-2 pr-4">Total</td>
                      <td className="pt-2 pr-4 text-right tabular-nums">
                        {totalUnitsDisplay}
                      </td>
                      <td className="pt-2 pr-4 text-right tabular-nums text-zinc-500">
                        {totalCallsDisplay}
                      </td>
                      <td />
                    </tr>
                  </tfoot>
                </table>
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}
