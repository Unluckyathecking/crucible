"use client";

import { Fragment, useState, useEffect, useCallback, useRef, useMemo } from "react";
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
import { fetchUsage, sanitizeError } from "@/lib/usage-fetch";
import { UsageChart } from "./usage-chart";

// Earliest date accepted by the date inputs and by parseDateParam.
const MIN_DATE_PARAM = "1970-01-01";

// BigInt sentinel reused by the totals memo; kept module-level so it is stable.
const MAX_SAFE_BI = BigInt(Number.MAX_SAFE_INTEGER);

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

interface UsageClientProps {
  initialFrom: string;
  initialTo: string;    // display-to (today inclusive, YYYY-MM-DD)
  initialApiTo: string; // exclusive upper bound for API (tomorrow, YYYY-MM-DD)
}

export function UsageClient({ initialFrom, initialTo, initialApiTo }: UsageClientProps) {
  const [displayFrom, setDisplayFrom] = useState(initialFrom);
  const [displayTo, setDisplayTo] = useState(initialTo);
  const [rangeError, setRangeError] = useState<string | null>(null);
  // Track the API params used for the active query so drill-down is consistent.
  const [queryFrom, setQueryFrom] = useState(initialFrom);
  const [queryTo, setQueryTo] = useState(initialApiTo);
  // Initialize to "loading" so the first render shows a loading indicator
  // immediately, without a flash of empty state before the useEffect fires.
  const [data, setData] = useState<DataState>({ status: "loading" });
  const [drill, setDrill] = useState<DrillState>({ status: "none" });
  // Refs for queryFrom/queryTo: captured inside async fetchUsage calls so they
  // must reflect the latest committed values, not a closure-stale snapshot.
  const queryFromRef = useRef(queryFrom);
  queryFromRef.current = queryFrom;
  const queryToRef = useRef(queryTo);
  queryToRef.current = queryTo;

  const abortRef = useRef<AbortController | null>(null);
  const drillAbortRef = useRef<AbortController | null>(null);
  const drillSeqRef = useRef(0);
  // generationRef: monotonically increasing, incremented on each loadMain call.
  // A stale fetch completing after a newer one has started is discarded: gen !== generationRef.current.
  const generationRef = useRef(0);

  const loadMain = useCallback(async (apiFrom: string, apiTo: string, signal?: AbortSignal) => {
    const gen = ++generationRef.current;
    setData({ status: "loading" });
    setDrill({ status: "none" });
    try {
      const result = await fetchUsage(apiFrom, apiTo, undefined, signal);
      // null: fetch was aborted (including component unmount — cleanup aborts the signal).
      // gen guard: discard responses from superseded fetches.
      if (result === null || gen !== generationRef.current) return;
      if ("error" in result) {
        setData({ status: "error", message: result.error });
        return;
      }
      setData({
        status: "ok",
        ops: aggregateByOperation(result.data),
        buckets: bucketByDay(result.data),
      });
    } catch (err) {
      if (gen !== generationRef.current) return;
      setData({ status: "error", message: err instanceof Error ? err.message : "Failed to load usage data" });
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    void loadMain(initialFrom, initialApiTo, ctrl.signal);
    return () => {
      abortRef.current?.abort();
      drillAbortRef.current?.abort();
    };
    // Intentionally run once on mount; loadMain is stable (useCallback with no deps).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function handleApply() {
    // Invalidate any in-flight drill-down before clearing state so a resolving
    // fetch cannot overwrite the "none" state with stale events.
    drillSeqRef.current++;
    setDrill({ status: "none" });
    drillAbortRef.current?.abort();
    const fromDate = parseDateParam(displayFrom);
    const toDate = parseDateParam(displayTo);
    if (isNaN(fromDate.getTime()) || isNaN(toDate.getTime())) {
      setRangeError("Invalid date");
      return;
    }
    const apiFrom = toISODateString(fromDate);
    // Compute the exclusive upper bound once; reuse for both validation and the API call.
    const apiToDate = new Date(toDate.getTime() + MS_PER_DAY);
    const apiTo = toISODateString(apiToDate);
    // Validate with the exclusive upper bound to match the API contract;
    // the user-visible maximum is MAX_USAGE_RANGE_DAYS inclusive days.
    const check = validateDateRange(fromDate, apiToDate);
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
    // Use drill state from the render closure (not a ref) for the toggle check:
    // handleDrillDown is recreated on each render, so drill is always current here.
    if ((drill.status === "ok" || drill.status === "loading") && drill.operation === operation) {
      // Invalidate before clearing state so any resolving fetch is discarded.
      drillSeqRef.current++;
      setDrill({ status: "none" });
      drillAbortRef.current?.abort();
      return;
    }
    drillAbortRef.current?.abort();
    const ctrl = new AbortController();
    drillAbortRef.current = ctrl;
    const seq = ++drillSeqRef.current;
    setDrill({ status: "loading", operation });
    try {
      const result = await fetchUsage(queryFromRef.current, queryToRef.current, operation, ctrl.signal);
      if (drillSeqRef.current !== seq) return;
      if (result === null) return;
      if ("error" in result) {
        setDrill({ status: "error", operation, message: result.error });
        return;
      }
      setDrill({ status: "ok", operation, events: result.data });
    } catch (err) {
      if (drillSeqRef.current !== seq) return;
      setDrill({ status: "error", operation, message: err instanceof Error ? err.message : "Failed to load events" });
    }
  }

  // todayStr comes from the server component (initialTo) — same value, no state needed.
  const todayStr = initialTo;

  // fromMax: use displayTo as the upper bound for from only when displayTo is valid
  // and does not exceed today, preventing from being set to a future date.
  const fromMax = useMemo(() => {
    const td = parseDateParam(displayTo);
    return !isNaN(td.getTime()) && td.getTime() <= parseDateParam(todayStr).getTime()
      ? displayTo : todayStr;
  }, [displayTo, todayStr]);

  const toMin = useMemo(() => {
    const fd = parseDateParam(displayFrom);
    // Use displayFrom as the minimum whenever it is a valid date, even if the
    // overall range is currently invalid. This prevents picking a to-date before from.
    if (!isNaN(fd.getTime())) return displayFrom;
    return MIN_DATE_PARAM;
  }, [displayFrom]);

  // Memoized so BigInt reduce doesn't run on unrelated re-renders (drill toggle, etc).
  // isRawEvent in usage-fetch.ts validates billable_units as a finite integer, so
  // BigInt() is safe without additional guards.
  const { totalUnitsDisplay, totalCallsDisplay } = useMemo(() => {
    if (data.status !== "ok") return { totalUnitsDisplay: "0", totalCallsDisplay: "0" };
    const totalUnitsBig = data.ops.reduce(
      (a, r) => a + BigInt(r.total_billable_units),
      0n,
    );
    const totalCallsBig = data.ops.reduce(
      (a, r) => a + BigInt(r.event_count),
      0n,
    );
    return {
      totalUnitsDisplay: totalUnitsBig > MAX_SAFE_BI ? "∞" : Number(totalUnitsBig).toLocaleString(),
      totalCallsDisplay: totalCallsBig > MAX_SAFE_BI ? "∞" : Number(totalCallsBig).toLocaleString(),
    };
  }, [data]);

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
              min={MIN_DATE_PARAM}
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
            disabled={data.status === "loading"}
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
          {sanitizeError(data.message)}
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
                        <Fragment key={row.operation}>
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
                            <tr key={`${row.operation}-drill`} className="bg-zinc-50">
                              <td colSpan={4} className="px-2 py-3">
                                {hasError && drill.status === "error" && (
                                  <p className="text-sm text-red-600">{sanitizeError(drill.message)}</p>
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
                                          {drill.events.map((e) => {
                                            const ts = new Date(e.created_at);
                                            return (
                                              <tr
                                                key={e.id}
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
                        </Fragment>
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
