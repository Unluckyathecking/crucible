import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  OperatorApiError,
  getCustomer,
  getCustomerUsage,
  listAuditEvents,
  listCustomers,
  listPlans,
} from "../client";

const TOKEN = "test-operator-token-that-is-at-least-32-bytes-long";
const GATEWAY = "http://gateway.internal:8080";
const VALID_UUID = "550e8400-e29b-41d4-a716-446655440000";

function mockFetchJson(status: number, body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

beforeEach(() => {
  process.env.OPERATOR_TOKEN = TOKEN;
  process.env.GATEWAY_URL = GATEWAY;
});

afterEach(() => {
  delete process.env.OPERATOR_TOKEN;
  delete process.env.GATEWAY_URL;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("operator client — bearer attachment (server-side only)", () => {
  it("attaches Authorization: Bearer <OPERATOR_TOKEN> to every gateway request", async () => {
    const fetchMock = mockFetchJson(200, { items: [], total: 0 });
    await listCustomers();
    const [, init] = fetchMock.mock.calls[0];
    expect((init.headers as Record<string, string>).Authorization).toBe(`Bearer ${TOKEN}`);
  });

  it("never sends the token as a URL query parameter", async () => {
    const fetchMock = mockFetchJson(200, { items: [], total: 0 });
    await listCustomers({ planId: "pro", page: 2, perPage: 10 });
    const [url] = fetchMock.mock.calls[0];
    expect(String(url)).not.toContain(TOKEN);
  });

  it("the returned Page result does not itself carry the token", async () => {
    mockFetchJson(200, { items: [{ id: VALID_UUID, email: "a@b.com", plan_id: "pro", created_at: "x", updated_at: "x" }], total: 1 });
    const result = await listCustomers();
    expect(JSON.stringify(result)).not.toContain(TOKEN);
  });
});

describe("listCustomers — Page<Customer> envelope validation", () => {
  it("returns items/total on a well-formed response", async () => {
    mockFetchJson(200, {
      items: [{ id: VALID_UUID, email: "a@b.com", plan_id: "pro", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z" }],
      total: 1,
    });
    const result = await listCustomers();
    expect(result.total).toBe(1);
    expect(result.items[0].email).toBe("a@b.com");
  });

  it("builds the gateway URL with plan_id/page/per_page query params", async () => {
    const fetchMock = mockFetchJson(200, { items: [], total: 0 });
    await listCustomers({ planId: "pro", page: 3, perPage: 50 });
    const [url] = fetchMock.mock.calls[0];
    const parsed = new URL(String(url));
    expect(parsed.origin + parsed.pathname).toBe(`${GATEWAY}/v1/admin/customers`);
    expect(parsed.searchParams.get("plan_id")).toBe("pro");
    expect(parsed.searchParams.get("page")).toBe("3");
    expect(parsed.searchParams.get("per_page")).toBe("50");
  });

  it("rejects a response missing items (malformed envelope)", async () => {
    mockFetchJson(200, { total: 5 });
    await expect(listCustomers()).rejects.toBeInstanceOf(OperatorApiError);
  });

  it("rejects a response missing total (malformed envelope)", async () => {
    mockFetchJson(200, { items: [] });
    await expect(listCustomers()).rejects.toBeInstanceOf(OperatorApiError);
  });

  it("rejects a response where items is not an array", async () => {
    mockFetchJson(200, { items: {}, total: 0 });
    await expect(listCustomers()).rejects.toBeInstanceOf(OperatorApiError);
  });

  it("surfaces the gateway's error message on a non-2xx response", async () => {
    mockFetchJson(401, { error: { status: 401, code: "UNAUTHORIZED", message: "invalid operator token" } });
    await expect(listCustomers()).rejects.toMatchObject({ message: "invalid operator token", status: 401 });
  });

  it("throws when OPERATOR_TOKEN is not configured", async () => {
    delete process.env.OPERATOR_TOKEN;
    mockFetchJson(200, { items: [], total: 0 });
    await expect(listCustomers()).rejects.toThrow(/OPERATOR_TOKEN/);
  });
});

describe("listAuditEvents — Page<AuditEvent> envelope validation", () => {
  it("returns a validated page and wires all filters into query params", async () => {
    const fetchMock = mockFetchJson(200, { items: [], total: 0 });
    await listAuditEvents({ customerId: VALID_UUID, action: "key.created", start: "2026-01-01T00:00:00Z", end: "2026-02-01T00:00:00Z", page: 1, perPage: 25 });
    const [url] = fetchMock.mock.calls[0];
    const parsed = new URL(String(url));
    expect(parsed.pathname).toBe("/v1/admin/audit");
    expect(parsed.searchParams.get("customer_id")).toBe(VALID_UUID);
    expect(parsed.searchParams.get("action")).toBe("key.created");
  });

  it("rejects a malformed audit response", async () => {
    mockFetchJson(200, { events: [] });
    await expect(listAuditEvents()).rejects.toBeInstanceOf(OperatorApiError);
  });
});

describe("listPlans — array validation (not paginated)", () => {
  it("returns the plan array on a well-formed response", async () => {
    mockFetchJson(200, [{ id: "pro", display_name: "Pro", rate_limit_per_minute: 60, created_at: "2026-01-01T00:00:00Z" }]);
    const plans = await listPlans();
    expect(plans).toHaveLength(1);
  });

  it("rejects a response that isn't an array", async () => {
    mockFetchJson(200, { items: [] });
    await expect(listPlans()).rejects.toBeInstanceOf(OperatorApiError);
  });
});

describe("getCustomer / getCustomerUsage — id validation", () => {
  it("rejects a non-UUID id without making a network call", async () => {
    const fetchMock = mockFetchJson(200, {});
    await expect(getCustomer("not-a-uuid")).rejects.toMatchObject({ status: 400 });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("returns a validated Customer for a well-formed response", async () => {
    mockFetchJson(200, { id: VALID_UUID, email: "a@b.com", plan_id: "pro", created_at: "x", updated_at: "x" });
    const customer = await getCustomer(VALID_UUID);
    expect(customer.email).toBe("a@b.com");
  });

  it("rejects a malformed Customer response", async () => {
    mockFetchJson(200, { id: VALID_UUID });
    await expect(getCustomer(VALID_UUID)).rejects.toBeInstanceOf(OperatorApiError);
  });

  it("rejects a malformed CustomerUsageResult response", async () => {
    mockFetchJson(200, { period_start: "x" });
    await expect(getCustomerUsage(VALID_UUID)).rejects.toBeInstanceOf(OperatorApiError);
  });

  it("returns a validated usage result for a well-formed response", async () => {
    mockFetchJson(200, {
      period_start: "2026-01-01T00:00:00Z",
      period_end: "2026-02-01T00:00:00Z",
      total_units: 100,
      total_calls: 10,
      breakdown: [{ operation: "op", total_units: 100, calls: 10 }],
    });
    const usage = await getCustomerUsage(VALID_UUID);
    expect(usage.breakdown).toHaveLength(1);
  });
});
