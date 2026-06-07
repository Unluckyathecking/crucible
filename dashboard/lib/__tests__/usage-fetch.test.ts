import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fetchUsage } from "@/lib/usage-fetch";

function mockResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("fetchUsage", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("returns data array on 200 OK with valid array JSON", async () => {
    const events = [{ id: "evt-1", operation: "search", billable_units: 5, created_at: "2024-01-01T00:00:00.000Z" }];
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, events));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ data: events });
  });

  it("returns empty data array on 200 OK with []", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ data: [] });
  });

  it("returns error for non-array 200 response", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, { not: "an array" }));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns error when array elements lack operation string field", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, [{ operation: 123 }]));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns data when id field is a string (required by isRawEvent)", async () => {
    const event = { id: "abc", operation: "search", billable_units: 5, created_at: "2024-01-01T00:00:00.000Z" };
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, [event]));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ data: [event] });
  });

  it("returns error when id field is a number (must be a string)", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(200, [{ id: 123, operation: "search", billable_units: 5, created_at: "2024-01-01T00:00:00.000Z" }]),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns error when id field is absent (isRawEvent requires id as string)", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(200, [{ operation: "search", billable_units: 5, created_at: "2024-01-01T00:00:00.000Z" }]),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns error for array containing a null element", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(200, [{ operation: "search", billable_units: 5, created_at: "2024-01-01T00:00:00.000Z" }, null]),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns error for array with NaN billable_units (Number.isFinite rejects NaN)", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(200, [{ operation: "search", billable_units: NaN, created_at: "2024-01-01T00:00:00.000Z" }]),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns error for array with Infinity billable_units (Number.isFinite rejects Infinity)", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(200, [{ operation: "search", billable_units: Infinity, created_at: "2024-01-01T00:00:00.000Z" }]),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("returns error for 401 Unauthorized", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(401, {}));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Session expired — please reload the page." });
  });

  it("returns error for 403 Forbidden", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(403, {}));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Forbidden — request rejected (403)." });
  });

  it("returns generic server error for non-string error body", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(500, { error: 42 }));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Server error (500)" });
  });

  it("returns generic server error when body is not an object", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(500, "plain text"));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Server error (500)" });
  });

  it("strips HTML-significant characters from server error messages", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(500, { error: "<script>alert('xss')</script>" }),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    if (!result || !("error" in result)) throw new Error("expected error result");
    expect(result.error).not.toContain("<");
    expect(result.error).not.toContain(">");
  });

  it("strips unclosed angle brackets from server error messages", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(
      mockResponse(500, { error: "<img onerror='alert(1)'" }),
    );
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    if (!result || !("error" in result)) throw new Error("expected error result");
    expect(result.error).not.toContain("<");
  });

  it("returns network error message when fetch throws a non-abort error", async () => {
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError("Failed to fetch"));
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Network error — please check your connection." });
  });

  it("returns null when fetch throws an AbortError", async () => {
    const abortErr = new Error("Aborted");
    abortErr.name = "AbortError";
    vi.mocked(fetch).mockRejectedValueOnce(abortErr);
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toBeNull();
  });

  it("returns null when the abort signal fires after fetch starts", async () => {
    vi.mocked(fetch).mockImplementation((_, options) => {
      return new Promise<Response>((_, reject) => {
        const signal = (options as RequestInit)?.signal;
        if (signal?.aborted) {
          const err = new Error("Aborted");
          err.name = "AbortError";
          reject(err);
          return;
        }
        signal?.addEventListener("abort", () => {
          const err = new Error("Aborted");
          err.name = "AbortError";
          reject(err);
        });
      });
    });
    const ctrl = new AbortController();
    const promise = fetchUsage("2024-01-01", "2024-02-01", undefined, ctrl.signal);
    ctrl.abort();
    const result = await promise;
    expect(result).toBeNull();
  });

  it("appends operation query param when provided", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    await fetchUsage("2024-01-01", "2024-02-01", "search");
    const url = (vi.mocked(fetch).mock.calls[0][0] as string);
    expect(url).toContain("operation=search");
  });

  it("omits operation query param when not provided", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    await fetchUsage("2024-01-01", "2024-02-01");
    const url = (vi.mocked(fetch).mock.calls[0][0] as string);
    expect(url).not.toContain("operation");
  });

  it("omits operation query param for empty-string operation (server rejects empty param with 400)", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    await fetchUsage("2024-01-01", "2024-02-01", "");
    const url = (vi.mocked(fetch).mock.calls[0][0] as string);
    expect(url).not.toContain("operation");
  });

  it("sends X-Requested-With: XMLHttpRequest header", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    await fetchUsage("2024-01-01", "2024-02-01");
    const options = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect((options.headers as Record<string, string>)["X-Requested-With"]).toBe("XMLHttpRequest");
  });

  it("sends Accept: application/json header", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    await fetchUsage("2024-01-01", "2024-02-01");
    const options = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect((options.headers as Record<string, string>)["Accept"]).toBe("application/json");
  });

  it("returns error when 200 response body is malformed JSON", async () => {
    const badJson = {
      status: 200,
      ok: true,
      json: () => Promise.reject(new SyntaxError("Unexpected token")),
    } as unknown as Response;
    vi.mocked(fetch).mockResolvedValueOnce(badJson);
    const result = await fetchUsage("2024-01-01", "2024-02-01");
    expect(result).toEqual({ error: "Unexpected response format from server" });
  });

  it("forwards the AbortSignal to fetch", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(mockResponse(200, []));
    const ctrl = new AbortController();
    await fetchUsage("2024-01-01", "2024-02-01", undefined, ctrl.signal);
    const options = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect(options.signal).toBe(ctrl.signal);
  });
});
