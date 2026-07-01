// Typed HTTP client for the gateway's read-only /v1/admin/* API
// (gateway/internal/operator/). Server-only: every export attaches OPERATOR_TOKEN
// as a Bearer header via requireOperatorToken(); never import this module from a
// "use client" component. Response shapes are validated at runtime — a gateway
// response that doesn't match the documented contract throws OperatorApiError
// instead of silently propagating malformed data to a page.
import { getGatewayUrl, requireOperatorToken } from "@/lib/env";
import { UUID_RE } from "@/lib/validation";

export interface Page<T> {
  items: T[];
  total: number;
}

export interface Customer {
  id: string;
  email: string;
  stripe_customer_id?: string;
  plan_id: string;
  created_at: string;
  updated_at: string;
}

export interface OperationUsage {
  operation: string;
  total_units: number;
  calls: number;
}

export interface CustomerUsageResult {
  period_start: string;
  period_end: string;
  total_units: number;
  total_calls: number;
  breakdown: OperationUsage[];
}

export interface AuditEvent {
  id: number;
  actor_type: string;
  actor_id?: string;
  action: string;
  target_type?: string;
  target_id?: string;
  details?: unknown;
  created_at: string;
}

export interface Plan {
  id: string;
  display_name: string;
  stripe_price_id?: string;
  rate_limit_per_minute: number;
  monthly_unit_cap?: number;
  created_at: string;
}

export class OperatorApiError extends Error {
  constructor(message: string, public status: number) {
    super(message);
    this.name = "OperatorApiError";
  }
}

function isPage<T>(value: unknown): value is Page<T> {
  return (
    typeof value === "object" &&
    value !== null &&
    Array.isArray((value as Record<string, unknown>).items) &&
    typeof (value as Record<string, unknown>).total === "number"
  );
}

function assertPage<T>(value: unknown, context: string): Page<T> {
  if (!isPage<T>(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected {items: [], total: number} envelope`, 502);
  }
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function assertCustomer(value: unknown, context: string): Customer {
  if (!isRecord(value) || typeof value.id !== "string" || typeof value.email !== "string" || typeof value.plan_id !== "string") {
    throw new OperatorApiError(`malformed response for ${context}: expected a Customer object`, 502);
  }
  return value as unknown as Customer;
}

function assertUsageResult(value: unknown, context: string): CustomerUsageResult {
  if (!isRecord(value) || !Array.isArray(value.breakdown) || typeof value.total_units !== "number") {
    throw new OperatorApiError(`malformed response for ${context}: expected a CustomerUsageResult object`, 502);
  }
  return value as unknown as CustomerUsageResult;
}

function assertPlanArray(value: unknown, context: string): Plan[] {
  if (!Array.isArray(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected an array of Plan`, 502);
  }
  return value as Plan[];
}

function validateUuid(id: string, context: string): void {
  if (!UUID_RE.test(id)) {
    throw new OperatorApiError(`invalid id for ${context}: not a UUID`, 400);
  }
}

async function operatorFetch(path: string, searchParams?: Record<string, string | undefined>): Promise<unknown> {
  const token = requireOperatorToken();
  const url = new URL(path, getGatewayUrl());
  if (searchParams) {
    for (const [key, value] of Object.entries(searchParams)) {
      if (value !== undefined && value !== "") url.searchParams.set(key, value);
    }
  }

  const res = await fetch(url, {
    headers: { Authorization: `Bearer ${token}` },
    cache: "no-store",
  });

  if (!res.ok) {
    let message = `gateway request to ${path} failed with status ${res.status}`;
    try {
      const body = (await res.json()) as { error?: { message?: string } };
      if (body?.error?.message) message = body.error.message;
    } catch {
      // response body wasn't JSON — fall back to the generic message above.
    }
    throw new OperatorApiError(message, res.status);
  }

  return res.json();
}

export interface ListCustomersParams {
  planId?: string;
  page?: number;
  perPage?: number;
}

export async function listCustomers(params: ListCustomersParams = {}): Promise<Page<Customer>> {
  const data = await operatorFetch("/v1/admin/customers", {
    plan_id: params.planId,
    page: params.page?.toString(),
    per_page: params.perPage?.toString(),
  });
  return assertPage<Customer>(data, "listCustomers");
}

export async function getCustomer(id: string): Promise<Customer> {
  validateUuid(id, "getCustomer");
  const data = await operatorFetch(`/v1/admin/customers/${encodeURIComponent(id)}`);
  return assertCustomer(data, "getCustomer");
}

export interface GetCustomerUsageParams {
  start?: string;
  end?: string;
}

export async function getCustomerUsage(id: string, params: GetCustomerUsageParams = {}): Promise<CustomerUsageResult> {
  validateUuid(id, "getCustomerUsage");
  const data = await operatorFetch(`/v1/admin/customers/${encodeURIComponent(id)}/usage`, {
    start: params.start,
    end: params.end,
  });
  return assertUsageResult(data, "getCustomerUsage");
}

export interface ListAuditEventsParams {
  customerId?: string;
  action?: string;
  start?: string;
  end?: string;
  page?: number;
  perPage?: number;
}

export async function listAuditEvents(params: ListAuditEventsParams = {}): Promise<Page<AuditEvent>> {
  const data = await operatorFetch("/v1/admin/audit", {
    customer_id: params.customerId,
    action: params.action,
    start: params.start,
    end: params.end,
    page: params.page?.toString(),
    per_page: params.perPage?.toString(),
  });
  return assertPage<AuditEvent>(data, "listAuditEvents");
}

export async function listPlans(): Promise<Plan[]> {
  const data = await operatorFetch("/v1/admin/plans");
  return assertPlanArray(data, "listPlans");
}
