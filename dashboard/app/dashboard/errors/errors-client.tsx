"use client";

import { useState, useEffect, useCallback, useRef } from "react";

interface ErrorEvent {
  id: string;
  operation: string;
  error_code: string;
  http_status: number;
  message: string;
  request_id: string;
  created_at: string;
  request_payload: string | null;
}

interface ApiResponse {
  data: ErrorEvent[];
  has_more: boolean;
  page: number;
  limit: number;
}

type LoadState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ok"; data: ErrorEvent[]; has_more: boolean; page: number };

const ISO_DATE_RE = /^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/;
const MS_PER_DAY = 86_400_000;
const MAX_RANGE_DAYS = 90;

function toISODate(d: Date): string {
  return d.toISOString().slice(0, 10);
}

async function fetchErrors(
  from: string,
  to: string,         // inclusive display date; the API adds one day for the exclusive DB bound
  operation: string,
  code: string,
  page: number,
  signal: AbortSignal,
): Promise<ApiResponse | { error: string } | null> {
  const params = new URLSearchParams({ from, to, page: String(page) });
  if (operation) params.set("operation", operation);
  if (code) params.set("code", code);
  try {
    const res = await fetch(`/api/errors?${params}`, {
      headers: { "X-Requested-With": "XMLHttpRequest" },
      signal,
    });
    const body = (await res.json()) as unknown;
    if (!res.ok) {
      const msg =
        typeof body === "object" && body !== null && "error" in body &&
        typeof (body as Record<string, unknown>).error === "string"
          ? (body as { error: string }).error
          : `HTTP ${res.status}`;
      return { error: msg };
    }
    if (
      typeof body !== "object" || body === null ||
      !Array.isArray((body as Record<string, unknown>).data) ||
      typeof (body as Record<string, unknown>).has_more !== "boolean" ||
      typeof (body as Record<string, unknown>).page !== "number" ||
      typeof (body as Record<string, unknown>).limit !== "number"
    ) {
      return { error: "Invalid response format" };
    }
    return body as ApiResponse;
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") return null;
    return { error: err instanceof Error ? err.message : "Network error" };
  }
}

function formatTs(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  try {
    return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC");
  } catch {
    return iso;
  }
}

interface ErrorsClientProps {
  initialFrom: string;
  initialTo: string;  // inclusive upper bound — sent directly to the API as 'to'
}

export function ErrorsClient({ initialFrom, initialTo }: ErrorsClientProps) {
  const [displayFrom, setDisplayFrom] = useState(initialFrom);
  const [displayTo, setDisplayTo] = useState(initialTo);
  const [operationFilter, setOperationFilter] = useState("");
  const [codeFilter, setCodeFilter] = useState("");
  const [rangeError, setRangeError] = useState<string | null>(null);
  const [state, setState] = useState<LoadState>({ status: "loading" });
  // Initialised to server-provided initialTo so the first paint matches the
  // server render (no hydration mismatch). The useEffect corrects to the
  // actual client-side today after hydration in case midnight crossed between
  // server render and client mount.
  const [todayUTC, setTodayUTC] = useState(initialTo);
  useEffect(() => {
    const now = new Date();
    const computed = toISODate(new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate())));
    if (computed !== initialTo) setTodayUTC(computed);
  }, [initialTo]);

  const abortRef = useRef<AbortController | null>(null);
  const generationRef = useRef<number>(0);
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  // queryRef: inclusive 'to' date used by the current main query.
  // Updated synchronously in handleApply so handlePrev/handleNext always read
  // the correct range even if clicked before the next React render commits.
  const queryRef = useRef({ from: initialFrom, to: initialTo, op: "", code: "" });

  const load = useCallback(
    async (from: string, to: string, op: string, code: string, page: number) => {
      abortRef.current?.abort();
      const ctrl = new AbortController();
      abortRef.current = ctrl;
      const gen = ++generationRef.current;
      // Set loading synchronously so the spinner appears immediately while the
      // fetch is in-flight. The gen check after the await guarantees a stale
      // response from a superseded request can never overwrite state.
      setState({ status: "loading" });
      const result = await fetchErrors(from, to, op, code, page, ctrl.signal);
      if (result === null || gen !== generationRef.current || !mountedRef.current) return;
      if ("error" in result) {
        setState({ status: "error", message: result.error });
        return;
      }
      setState({ status: "ok", data: result.data, has_more: result.has_more, page: result.page });
    },
    [],
  );

  useEffect(() => {
    queryRef.current = { from: initialFrom, to: initialTo, op: "", code: "" };
    void load(initialFrom, initialTo, "", "", 1);
    return () => { abortRef.current?.abort(); };
  }, [initialFrom, initialTo, load]);

  const handleApply = useCallback(() => {
    if (!ISO_DATE_RE.test(displayFrom) || !ISO_DATE_RE.test(displayTo)) {
      setRangeError("Invalid date");
      return;
    }
    const fromMs = new Date(displayFrom + "T00:00:00.000Z").getTime();
    const toMs = new Date(displayTo + "T00:00:00.000Z").getTime();
    if (fromMs > toMs) {
      setRangeError("'From' must not be after 'To'");
      return;
    }
    if (toMs - fromMs > MAX_RANGE_DAYS * MS_PER_DAY) {
      setRangeError(`Max date range is ${MAX_RANGE_DAYS} days`);
      return;
    }
    setRangeError(null);
    // Pass the inclusive display date directly to the API; the API converts it
    // to an exclusive midnight bound server-side (+1 day).
    queryRef.current = { from: displayFrom, to: displayTo, op: operationFilter, code: codeFilter };
    void load(displayFrom, displayTo, operationFilter, codeFilter, 1);
  }, [displayFrom, displayTo, operationFilter, codeFilter, load]);

  const handlePrev = useCallback(() => {
    if (state.status !== "ok" || state.page <= 1) return;
    const { from, to, op, code } = queryRef.current;
    void load(from, to, op, code, state.page - 1);
  }, [state, load]);

  const handleNext = useCallback(() => {
    if (state.status !== "ok" || !state.has_more) return;
    const { from, to, op, code } = queryRef.current;
    void load(from, to, op, code, state.page + 1);
  }, [state, load]);

  return (
    <div className="space-y-5">
      {/* Filters */}
      <section className="border border-zinc-200 rounded-lg p-4 sm:p-5">
        <h2 className="text-base font-semibold mb-3">Filters</h2>
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-zinc-500">From</span>
            <input
              type="date"
              value={displayFrom}
              max={todayUTC}
              onChange={(e) => { setDisplayFrom(e.target.value); setRangeError(null); }}
              className="border border-zinc-200 rounded px-2 py-1 text-sm bg-white"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-zinc-500">To (inclusive)</span>
            <input
              type="date"
              value={displayTo}
              min={displayFrom}
              max={todayUTC}
              onChange={(e) => { setDisplayTo(e.target.value); setRangeError(null); }}
              className="border border-zinc-200 rounded px-2 py-1 text-sm bg-white"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-zinc-500">Operation</span>
            <input
              type="text"
              value={operationFilter}
              placeholder="e.g. /v1/echo"
              onChange={(e) => setOperationFilter(e.target.value)}
              className="border border-zinc-200 rounded px-2 py-1 text-sm bg-white w-36"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="text-zinc-500">Error code</span>
            <input
              type="text"
              value={codeFilter}
              placeholder="e.g. RATE_LIMITED"
              onChange={(e) => setCodeFilter(e.target.value)}
              className="border border-zinc-200 rounded px-2 py-1 text-sm bg-white w-40"
            />
          </label>
          <button
            onClick={handleApply}
            disabled={state.status === "loading"}
            aria-busy={state.status === "loading"}
            className="px-3 py-1.5 bg-zinc-900 text-white text-sm rounded hover:bg-zinc-700 disabled:opacity-50"
          >
            {state.status === "loading" ? "Loading…" : "Apply"}
          </button>
        </div>
        {rangeError && (
          <p className="mt-2 text-sm text-red-600" role="alert">{rangeError}</p>
        )}
        <p className="mt-2 text-xs text-zinc-400">Max {MAX_RANGE_DAYS} days · error codes are uppercase (e.g. RATE_LIMITED)</p>
      </section>

      {state.status === "error" && (
        <p className="text-sm text-red-600" role="alert">{state.message}</p>
      )}

      {state.status === "ok" && (
        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5" aria-label="Error history">
          <h2 className="text-base font-semibold mb-3">Error history</h2>
          {state.data.length === 0 ? (
            <p className="text-sm text-zinc-500">No errors in this period.</p>
          ) : (
            <>
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="text-left text-zinc-500 border-b border-zinc-200">
                      <th scope="col" className="pb-2 pr-3 font-medium">Time (UTC)</th>
                      <th scope="col" className="pb-2 pr-3 font-medium">Operation</th>
                      <th scope="col" className="pb-2 pr-3 font-medium">Code</th>
                      <th scope="col" className="pb-2 pr-3 font-medium text-right">Status</th>
                      <th scope="col" className="pb-2 pr-3 font-medium">Message</th>
                      <th scope="col" className="pb-2 pr-3 font-medium">Request ID</th>
                      <th scope="col" className="pb-2 font-medium">Payload</th>
                    </tr>
                  </thead>
                  <tbody>
                    {state.data.map((e) => (
                      <tr key={e.id} className="border-b border-zinc-100 align-top">
                        <td className="py-2 pr-3 font-mono text-xs whitespace-nowrap">
                          {formatTs(e.created_at)}
                        </td>
                        <td className="py-2 pr-3 font-mono text-xs break-all">{e.operation}</td>
                        <td className="py-2 pr-3 font-mono text-xs whitespace-nowrap">{e.error_code}</td>
                        <td className="py-2 pr-3 text-right tabular-nums text-xs">{e.http_status}</td>
                        <td className="py-2 pr-3 text-xs text-zinc-600 max-w-xs truncate">{e.message}</td>
                        <td className="py-2 pr-3 font-mono text-xs text-zinc-400 break-all">{e.request_id}</td>
                        <td className="py-2 font-mono text-xs text-zinc-400 max-w-xs truncate" title={e.request_payload ?? undefined}>
                          {e.request_payload ?? <span className="text-zinc-300">—</span>}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {/* Pagination */}
              <div className="flex items-center justify-between mt-3">
                <button
                  onClick={handlePrev}
                  disabled={state.page <= 1}
                  className="text-sm text-zinc-500 hover:text-zinc-900 disabled:opacity-40"
                >
                  ← Prev
                </button>
                <span className="text-xs text-zinc-400">Page {state.page}</span>
                <button
                  onClick={handleNext}
                  disabled={!state.has_more}
                  className="text-sm text-zinc-500 hover:text-zinc-900 disabled:opacity-40"
                >
                  Next →
                </button>
              </div>
            </>
          )}
        </section>
      )}
    </div>
  );
}
