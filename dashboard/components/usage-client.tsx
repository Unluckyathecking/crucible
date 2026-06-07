"use client";

import React, { useState, useEffect, useCallback } from "react";
import {
  validateDateRange,
  parseDateParam,
  toISODateString,
  bucketByDay,
  aggregateByOperation,
  MAX_USAGE_RANGE_DAYS,
  MS_PER_DAY,
  type RawEvent,
  type DayBucket,
  type OperationRow,
} from "@/lib/usage-format";
import { UsageChart } from "./usage-chart";

async function fetchUsage(
  from: string,
  to: string,
  operation?: string,
): Promise<{ data: RawEvent[] } | { error: string }> {
  const params = new URLSearchParams({ from, to });
  if (operation) params.set("operation", operation);
  let res: Response;
  try {
    res = await fetch(`/api/usage?${params}`, {
      headers: { "X-Requested-With": "XMLHttpRequest" },
      cache: "no-store",
    });
  } catch {
    return { error: "Network error — please check your connection." };
  }
  if (res.status === 403) {
    // The header should always be sent; 403 means something stripped it (proxy, extension).
    return { error: "Forbidden (403) — the X-Requested-With header was stripped by your environment." };
  }
  if (res.status === 401) {
    return { error: "Session expired — please reload the page." };
  }
  if (!res.ok) {
    const body: unknown = await res.json().catch(() => ({}));
    if (typeof body !== "object" || body === null) {
      return { error: `Server error (${res.status})` };
    }
    const err = (body as Record<string, unknown>).error;
    return { error: typeof err === "string" ? err : `Server error (${res.status})` };
  }
  const json: unknown = await res.json();
  if (!Array.isArray(json)) {
    return { error: "Unexpected response format from server" };
  }
  return { data: json as RawEvent[] };
}

function utcTodayStr(): string {
  const d = new Date();
  return toISODateString(
    new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate())),
  );
}

// displayTo is the user-visible inclusive end date.
// The API's 'to' is exclusive, so add 1 day.
function toApiTo(displayTo: string): string {
  return toISODateString(new Date(parseDateParam(displayTo).getTime() + MS_PER_DAY));
}

function initRange(): { from: string; to: string } {
  const today = new Date();
  const todayUTC = new Date(
    Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate()),
  );
  return {
    from: toISODateString(new Date(todayUTC.getTime() - 29 * MS_PER_DAY)),
    to: toISODateString(todayUTC),
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
  const init = initRange();
  const [displayFrom, setDisplayFrom] = useState(init.from);
  const [displayTo, setDisplayTo] = useState(init.to);
  const [rangeError, setRangeError] = useState<string | null>(null);
  // Track the API params used for the active query so drill-down is consistent.
  const [queryFrom, setQueryFrom] = useState(init.from);
  const [queryTo, setQueryTo] = useState(() => toApiTo(init.to));
  const [data, setData] = useState<DataState>({ status: "idle" });
  const [drill, setDrill] = useState<DrillState>({ status: "none" });

  const loadMain = useCallback(async (apiFrom: string, apiTo: string) => {
    setData({ status: "loading" });
    setDrill({ status: "none" });
    const result = await fetchUsage(apiFrom, apiTo);
    if ("error" in result) {
      setData({ status: "error", message: result.error });
      return;
    }
    setData({
      status: "ok",
      ops: aggregateByOperation(result.data),
      buckets: bucketByDay(result.data),
    });
  }, []);

  useEffect(() => {
    const { from, to } = initRange();
    loadMain(from, toApiTo(to));
    // Intentionally run once on mount; loadMain is stable (useCallback with no deps).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function handleApply() {
    const apiFrom = displayFrom;
    const apiTo = toApiTo(displayTo);
    const check = validateDateRange(parseDateParam(apiFrom), parseDateParam(apiTo));
    if (!check.valid) {
      setRangeError(check.error ?? "Invalid date range");
      return;
    }
    setRangeError(null);
    setQueryFrom(apiFrom);
    setQueryTo(apiTo);
    loadMain(apiFrom, apiTo);
  }

  async function handleDrillDown(operation: string) {
    if (drill.status === "loading") return;
    if (drill.status === "ok" && drill.operation === operation) {
      setDrill({ status: "none" });
      return;
    }
    setDrill({ status: "loading", operation });
    const result = await fetchUsage(queryFrom, queryTo, operation);
    if ("error" in result) {
      setDrill({ status: "error", operation, message: result.error });
    } else {
      setDrill({ status: "ok", operation, events: result.data });
    }
  }

  const todayStr = utcTodayStr();

  // Mirror the BigInt saturation pattern from app/dashboard/page.tsx to guard against
  // Number.MAX_SAFE_INTEGER overflow when a customer has many operations.
  const _cap = BigInt(Number.MAX_SAFE_INTEGER);
  const _rawUnits =
    data.status === "ok"
      ? data.ops.reduce((a, r) => a + BigInt(r.total_billable_units), BigInt(0))
      : BigInt(0);
  const _rawCalls =
    data.status === "ok"
      ? data.ops.reduce((a, r) => a + BigInt(r.event_count), BigInt(0))
      : BigInt(0);
  const totalUnits = _rawUnits > _cap ? Number.MAX_SAFE_INTEGER : Number(_rawUnits);
  const totalCalls = _rawCalls > _cap ? Number.MAX_SAFE_INTEGER : Number(_rawCalls);

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
              max={displayTo || todayStr}
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
              min={displayFrom || "1970-01-01"}
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
                                          {drill.events.map((e, i) => (
                                            <tr key={`${e.created_at}-${e.billable_units}-${e.operation}-${i}`} className="border-b border-zinc-100">
                                              <td className="py-1 pr-4 font-mono">
                                                {new Date(e.created_at).toISOString()}
                                              </td>
                                              <td className="py-1 text-right tabular-nums">
                                                {e.billable_units.toLocaleString()}
                                              </td>
                                            </tr>
                                          ))}
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
                        {totalUnits.toLocaleString()}
                      </td>
                      <td className="pt-2 pr-4 text-right tabular-nums text-zinc-500">
                        {totalCalls.toLocaleString()}
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
