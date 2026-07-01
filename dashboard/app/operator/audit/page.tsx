import Link from "next/link";
import { OperatorNav } from "../_components/operator-nav";
import { Pagination } from "../_components/pagination";
import { listAuditEvents } from "@/lib/operator/client";

export const dynamic = "force-dynamic";

const PER_PAGE = 25;

interface AuditPageProps {
  searchParams: Promise<{
    customer_id?: string;
    action?: string;
    start?: string;
    end?: string;
    page?: string;
  }>;
}

export default async function OperatorAuditPage({ searchParams }: AuditPageProps) {
  const params = await searchParams;
  const page = Math.max(1, Number(params.page) || 1);

  const events = await listAuditEvents({
    customerId: params.customer_id,
    action: params.action,
    start: params.start,
    end: params.end,
    page,
    perPage: PER_PAGE,
  });

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-5xl">
        <OperatorNav />
        <h1 className="text-2xl font-bold mb-4">Audit log</h1>

        <form method="GET" className="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-4 text-sm items-end">
          <div className="flex flex-col gap-1">
            <label htmlFor="customer_id" className="text-zinc-600 dark:text-zinc-400">
              Customer ID
            </label>
            <input
              id="customer_id"
              name="customer_id"
              defaultValue={params.customer_id ?? ""}
              className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
            />
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor="action" className="text-zinc-600 dark:text-zinc-400">
              Action
            </label>
            <input
              id="action"
              name="action"
              defaultValue={params.action ?? ""}
              className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
            />
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor="start" className="text-zinc-600 dark:text-zinc-400">
              Start (RFC3339)
            </label>
            <input
              id="start"
              name="start"
              defaultValue={params.start ?? ""}
              placeholder="2026-06-01T00:00:00Z"
              className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
            />
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor="end" className="text-zinc-600 dark:text-zinc-400">
              End (RFC3339)
            </label>
            <input
              id="end"
              name="end"
              defaultValue={params.end ?? ""}
              placeholder="2026-07-01T00:00:00Z"
              className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
            />
          </div>
          <button type="submit" className="px-3 py-1 border border-zinc-300 dark:border-zinc-600 rounded col-span-2 sm:col-span-1">
            Filter
          </button>
        </form>

        <div className="overflow-x-auto border border-zinc-200 dark:border-zinc-700 rounded-lg">
          <table className="w-full text-sm">
            <thead className="bg-zinc-50 dark:bg-zinc-800 text-left">
              <tr>
                <th className="px-3 py-2">Time</th>
                <th className="px-3 py-2">Actor</th>
                <th className="px-3 py-2">Action</th>
                <th className="px-3 py-2">Target</th>
              </tr>
            </thead>
            <tbody>
              {events.items.map((event) => (
                <tr key={event.id} className="border-t border-zinc-200 dark:border-zinc-700">
                  <td className="px-3 py-2 whitespace-nowrap">{new Date(event.created_at).toLocaleString()}</td>
                  <td className="px-3 py-2">
                    {event.actor_id ? (
                      <Link href={`/operator/customers/${event.actor_id}`} className="underline">
                        {event.actor_type}
                      </Link>
                    ) : (
                      event.actor_type
                    )}
                  </td>
                  <td className="px-3 py-2">{event.action}</td>
                  <td className="px-3 py-2">{event.target_type ? `${event.target_type} ${event.target_id ?? ""}` : "—"}</td>
                </tr>
              ))}
              {events.items.length === 0 && (
                <tr>
                  <td className="px-3 py-4 text-zinc-500" colSpan={4}>
                    No audit events found.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        <Pagination basePath="/operator/audit" page={page} perPage={PER_PAGE} total={events.total} searchParams={params} />
      </div>
    </main>
  );
}
