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

export interface AdminJobError {
  code: string;
  message: string;
}

// AdminJob is the cross-customer job view from GET /v1/admin/jobs* — unlike
// the customer-facing job shape, it carries customer_id and claimed_by/
// claimed_at (gateway/internal/operator's adminJobItem).
export interface AdminJob {
  job_id: string;
  customer_id: string;
  operation: string;
  status: string;
  result?: unknown;
  billable_units?: number;
  units_label?: string;
  error?: AdminJobError;
  claimed_by?: string;
  claimed_at?: string;
  created_at: string;
  updated_at: string;
}

export interface ReleaseJobsResult {
  released: number;
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

// assertPage validates both the envelope and every item in it — the gateway
// contract promises Page<T>.items are all T, but only checking the envelope
// (items is an array, total is a number) lets a single malformed row through
// to render blank/garbled operator rows instead of failing loudly.
function assertPage<T>(value: unknown, context: string, itemGuard: (item: unknown) => item is T): Page<T> {
  if (!isPage<T>(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected {items: [], total: number} envelope`, 502);
  }
  if (!value.items.every(itemGuard)) {
    throw new OperatorApiError(`malformed response for ${context}: an item in the page failed shape validation`, 502);
  }
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

// isSafeCount guards usage/audit counters that originate as Go int64 (Postgres
// BIGINT) on the wire. The gateway doesn't string-encode these (unlike
// lib/db.ts's ::text-cast + saturateBigIntString pattern for direct Postgres
// reads), so by the time fetch's res.json() has parsed the body, a value
// outside Number.isSafeInteger range may already be silently rounded —
// rejecting it here means we never display a number we can't vouch for.
function isSafeCount(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function isCustomerShape(value: unknown): value is Customer {
  return (
    isRecord(value) &&
    typeof value.id === "string" &&
    typeof value.email === "string" &&
    typeof value.plan_id === "string" &&
    typeof value.created_at === "string" &&
    typeof value.updated_at === "string"
  );
}

function isAuditEventShape(value: unknown): value is AuditEvent {
  return (
    isRecord(value) &&
    typeof value.id === "number" &&
    typeof value.actor_type === "string" &&
    typeof value.action === "string" &&
    typeof value.created_at === "string"
  );
}

function isOperationUsageShape(value: unknown): value is OperationUsage {
  return isRecord(value) && typeof value.operation === "string" && isSafeCount(value.total_units) && isSafeCount(value.calls);
}

function isCustomerUsageResultShape(value: unknown): value is CustomerUsageResult {
  return (
    isRecord(value) &&
    typeof value.period_start === "string" &&
    typeof value.period_end === "string" &&
    isSafeCount(value.total_units) &&
    isSafeCount(value.total_calls) &&
    Array.isArray(value.breakdown) &&
    value.breakdown.every(isOperationUsageShape)
  );
}

function isAdminJobErrorShape(value: unknown): value is AdminJobError {
  return isRecord(value) && typeof value.code === "string" && typeof value.message === "string";
}

function isAdminJobShape(value: unknown): value is AdminJob {
  return (
    isRecord(value) &&
    typeof value.job_id === "string" &&
    typeof value.customer_id === "string" &&
    typeof value.operation === "string" &&
    typeof value.status === "string" &&
    typeof value.created_at === "string" &&
    typeof value.updated_at === "string" &&
    (value.error === undefined || isAdminJobErrorShape(value.error)) &&
    (value.claimed_by === undefined || typeof value.claimed_by === "string") &&
    (value.claimed_at === undefined || typeof value.claimed_at === "string")
  );
}

function isPlanShape(value: unknown): value is Plan {
  return (
    isRecord(value) &&
    typeof value.id === "string" &&
    typeof value.display_name === "string" &&
    typeof value.rate_limit_per_minute === "number" &&
    Number.isSafeInteger(value.rate_limit_per_minute) &&
    typeof value.created_at === "string" &&
    (value.monthly_unit_cap === undefined || value.monthly_unit_cap === null || isSafeCount(value.monthly_unit_cap))
  );
}

function assertCustomer(value: unknown, context: string): Customer {
  if (!isCustomerShape(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected a Customer object`, 502);
  }
  return value;
}

function assertUsageResult(value: unknown, context: string): CustomerUsageResult {
  if (!isCustomerUsageResultShape(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected a CustomerUsageResult object`, 502);
  }
  return value;
}

function assertPlanArray(value: unknown, context: string): Plan[] {
  if (!Array.isArray(value) || !value.every(isPlanShape)) {
    throw new OperatorApiError(`malformed response for ${context}: expected an array of Plan`, 502);
  }
  return value;
}

function assertAdminJob(value: unknown, context: string): AdminJob {
  if (!isAdminJobShape(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected an AdminJob object`, 502);
  }
  return value;
}

function isReleaseJobsResultShape(value: unknown): value is ReleaseJobsResult {
  return isRecord(value) && isSafeCount(value.released);
}

function assertReleaseJobsResult(value: unknown, context: string): ReleaseJobsResult {
  if (!isReleaseJobsResultShape(value)) {
    throw new OperatorApiError(`malformed response for ${context}: expected {released: number}`, 502);
  }
  return value;
}

function validateUuid(id: string, context: string): void {
  if (!UUID_RE.test(id)) {
    throw new OperatorApiError(`invalid id for ${context}: not a UUID`, 400);
  }
}

// operatorFetch is the only place requireOperatorToken() is called — every
// exported function in this module (GET reads and POST actions alike) routes
// through here so the bearer header is attached in exactly one spot.
async function operatorFetch(
  path: string,
  opts: { searchParams?: Record<string, string | undefined>; method?: string; body?: unknown } = {},
): Promise<unknown> {
  const token = requireOperatorToken();
  const url = new URL(path, getGatewayUrl());
  if (opts.searchParams) {
    for (const [key, value] of Object.entries(opts.searchParams)) {
      if (value !== undefined && value !== "") url.searchParams.set(key, value);
    }
  }

  const res = await fetch(url, {
    method: opts.method ?? "GET",
    headers: {
      Authorization: `Bearer ${token}`,
      ...(opts.body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
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

async function operatorPost(path: string, body?: unknown): Promise<unknown> {
  return operatorFetch(path, { method: "POST", body });
}

export interface ListCustomersParams {
  planId?: string;
  page?: number;
  perPage?: number;
}

export async function listCustomers(params: ListCustomersParams = {}): Promise<Page<Customer>> {
  const data = await operatorFetch("/v1/admin/customers", {
    searchParams: {
      plan_id: params.planId,
      page: params.page?.toString(),
      per_page: params.perPage?.toString(),
    },
  });
  return assertPage<Customer>(data, "listCustomers", isCustomerShape);
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
    searchParams: { start: params.start, end: params.end },
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
    searchParams: {
      customer_id: params.customerId,
      action: params.action,
      start: params.start,
      end: params.end,
      page: params.page?.toString(),
      per_page: params.perPage?.toString(),
    },
  });
  return assertPage<AuditEvent>(data, "listAuditEvents", isAuditEventShape);
}

export async function listPlans(): Promise<Plan[]> {
  const data = await operatorFetch("/v1/admin/plans");
  return assertPlanArray(data, "listPlans");
}

export interface ListAdminJobsParams {
  status?: string;
  page?: number;
  perPage?: number;
}

export async function listAdminJobs(params: ListAdminJobsParams = {}): Promise<Page<AdminJob>> {
  const data = await operatorFetch("/v1/admin/jobs", {
    searchParams: {
      status: params.status,
      page: params.page?.toString(),
      per_page: params.perPage?.toString(),
    },
  });
  return assertPage<AdminJob>(data, "listAdminJobs", isAdminJobShape);
}

// requeueJob flips a claimed/failed/dead-lettered job back to queued —
// see gateway/internal/jobs.Store.Requeue's doc comment on why this is only
// safe once the caller has positively confirmed no worker is still
// processing the job.
export async function requeueJob(id: string): Promise<AdminJob> {
  validateUuid(id, "requeueJob");
  const data = await operatorPost(`/v1/admin/jobs/${encodeURIComponent(id)}/requeue`);
  return assertAdminJob(data, "requeueJob");
}

// releaseJobs force-releases every job claimed by instanceId back to queued —
// see gateway/internal/jobs.Store.ReleaseClaimed's doc comment on why this is
// only safe once the caller has positively confirmed that instance is dead.
export async function releaseJobs(instanceId: string): Promise<ReleaseJobsResult> {
  validateUuid(instanceId, "releaseJobs");
  const data = await operatorPost("/v1/admin/jobs/release", { instance_id: instanceId });
  return assertReleaseJobsResult(data, "releaseJobs");
}
