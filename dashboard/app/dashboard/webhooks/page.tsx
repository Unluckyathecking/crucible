import { auth } from "@/auth";
import { redirect } from "next/navigation";
import Link from "next/link";
import {
  ensureCustomer,
  listWebhookEndpoints,
  listWebhookDeliveries,
  WebhookEndpointRow,
  WebhookDeliveryRow,
  WEBHOOK_EVENT_TYPES,
} from "@/lib/db";
import {
  WebhooksFormClient,
  RevokeEndpointButton,
  EditSubscriptionButton,
} from "./webhooks-client";

export const dynamic = "force-dynamic";

function statusBadge(status: string): string {
  switch (status) {
    case "delivered":
      return "text-green-700 bg-green-50 border-green-200";
    case "dead_letter":
      return "text-red-700 bg-red-50 border-red-200";
    case "delivering":
      return "text-yellow-700 bg-yellow-50 border-yellow-200";
    default:
      return "text-zinc-600 bg-zinc-50 border-zinc-200";
  }
}

function formatDate(d: Date): string {
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone: "UTC",
  });
}

export default async function WebhooksPage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }

  const customer = await ensureCustomer(session.user.email);
  const [endpoints, deliveries] = await Promise.all([
    listWebhookEndpoints(customer.id),
    listWebhookDeliveries(customer.id),
  ]);

  return (
    <main
      id="main-content"
      className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8"
    >
      <div className="mx-auto w-full max-w-5xl">
        <header className="flex items-center gap-4 mb-6 sm:mb-8">
          <Link
            href="/dashboard"
            className="text-sm text-zinc-500 hover:text-zinc-900"
          >
            ← Dashboard
          </Link>
          <h1 className="text-2xl sm:text-3xl font-bold">Webhooks</h1>
        </header>

        {/* ── Endpoint registration ─────────────────────────────────────── */}
        <section className="mb-8">
          <h2 className="text-lg font-semibold mb-3">Registered endpoints</h2>
          <p className="text-sm text-zinc-500 mb-4">
            Each registered HTTPS endpoint receives a signed POST for every
            event your product emits. The signing secret is shown once on
            creation — store it securely and verify{" "}
            <code className="font-mono text-xs">X-Crucible-Signature</code>{" "}
            on every incoming request.
          </p>

          {/* Add endpoint form (client component for secret reveal-once UX) */}
          <div className="mb-4 rounded-lg border border-zinc-200 p-4">
            <h3 className="text-sm font-medium mb-2">Add endpoint</h3>
            <WebhooksFormClient eventTypes={WEBHOOK_EVENT_TYPES} />
          </div>

          {/* Endpoint list */}
          {endpoints.length === 0 ? (
            <p className="text-sm text-zinc-400">No active endpoints.</p>
          ) : (
            <div className="rounded-lg border border-zinc-200 divide-y divide-zinc-100">
              {endpoints.map((ep: WebhookEndpointRow) => (
                <div
                  key={ep.id}
                  className="flex flex-col sm:flex-row sm:items-start justify-between gap-2 px-4 py-3"
                >
                  <div>
                    <span className="font-mono text-sm break-all">
                      {ep.url}
                    </span>
                    <p className="text-xs text-zinc-400 mt-0.5">
                      Added {formatDate(ep.created_at)} · ID:{" "}
                      <code className="font-mono">{ep.id.slice(0, 8)}…</code>
                    </p>
                    <p className="text-xs text-zinc-500 mt-1">
                      Subscribed:{" "}
                      {ep.subscribed_events === null
                        ? "All events"
                        : ep.subscribed_events.length === 0
                          ? "None"
                          : ep.subscribed_events.join(", ")}
                    </p>
                  </div>
                  <div className="shrink-0 flex flex-col items-end gap-2">
                    <RevokeEndpointButton endpointId={ep.id} />
                    <EditSubscriptionButton
                      endpointId={ep.id}
                      eventTypes={WEBHOOK_EVENT_TYPES}
                      current={ep.subscribed_events}
                    />
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        {/* ── Delivery log ──────────────────────────────────────────────── */}
        <section>
          <h2 className="text-lg font-semibold mb-3">Delivery log</h2>
          {deliveries.length === 0 ? (
            <p className="text-sm text-zinc-400">No deliveries yet.</p>
          ) : (
            <div className="overflow-x-auto rounded-lg border border-zinc-200">
              <table className="w-full text-sm">
                <thead className="bg-zinc-50 text-zinc-600 text-xs uppercase tracking-wide">
                  <tr>
                    <th className="px-4 py-2 text-left">Event ID</th>
                    <th className="px-4 py-2 text-left">Endpoint</th>
                    <th className="px-4 py-2 text-left">Status</th>
                    <th className="px-4 py-2 text-right">Attempts</th>
                    <th className="px-4 py-2 text-right">HTTP</th>
                    <th className="px-4 py-2 text-right">Time (UTC)</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-zinc-100">
                  {deliveries.map((d: WebhookDeliveryRow) => (
                    <tr key={d.id} className="hover:bg-zinc-50">
                      <td className="px-4 py-2 font-mono text-xs text-zinc-500">
                        {d.event_id.slice(0, 8)}…
                      </td>
                      <td className="px-4 py-2 max-w-[200px] truncate text-zinc-700">
                        {d.endpoint_url}
                      </td>
                      <td className="px-4 py-2">
                        <span
                          className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${statusBadge(d.status)}`}
                        >
                          {d.status}
                        </span>
                      </td>
                      <td className="px-4 py-2 text-right text-zinc-600">
                        {d.attempts}
                      </td>
                      <td className="px-4 py-2 text-right text-zinc-600">
                        {d.last_response_code ?? "—"}
                      </td>
                      <td className="px-4 py-2 text-right text-zinc-400 text-xs whitespace-nowrap">
                        {formatDate(d.created_at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      </div>
    </main>
  );
}
