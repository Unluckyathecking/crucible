import Link from "next/link";
import { OperatorNav } from "../_components/operator-nav";
import { Pagination } from "../_components/pagination";
import { replayDeadLetterAction, replayEndpointAction } from "./actions";
import { OperatorApiError, listDeadLetters } from "@/lib/operator/client";
import type { DeadLetterDelivery, Page } from "@/lib/operator/client";

export const dynamic = "force-dynamic";

const PER_PAGE = 25;

interface WebhooksPageProps {
  searchParams: Promise<{
    page?: string;
    error?: string;
    replayed?: string;
  }>;
}

export default async function OperatorWebhooksPage({ searchParams }: WebhooksPageProps) {
  const params = await searchParams;
  const page = Math.max(1, Number(params.page) || 1);

  let deadLetters: Page<DeadLetterDelivery> | null = null;
  let filterError: string | null = null;
  try {
    deadLetters = await listDeadLetters({ page, perPage: PER_PAGE });
  } catch (err) {
    // Only the gateway's documented page-too-large validation failure is a
    // user-fixable input error worth showing inline, same treatment as the
    // jobs page's filterError. Anything else re-throws to the error boundary.
    if (err instanceof OperatorApiError && err.status === 400) {
      filterError = err.message;
    } else {
      throw err;
    }
  }

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-6xl">
        <OperatorNav />
        <h1 className="text-2xl font-bold mb-4">Webhook dead-letters</h1>

        {params.error && (
          <p className="text-sm text-red-600 dark:text-red-400 mb-4" role="alert">
            {params.error}
          </p>
        )}
        {params.replayed !== undefined && (
          <p className="text-sm text-green-700 dark:text-green-400 mb-4" role="status">
            Replayed {params.replayed} delivery(ies).
          </p>
        )}

        {filterError ? (
          <p className="text-sm text-red-600 dark:text-red-400" role="alert">
            {filterError}{" "}
            <Link href="/operator/webhooks" className="underline">
              Reset filters
            </Link>
          </p>
        ) : (
          deadLetters && (
            <>
              <div className="overflow-x-auto border border-zinc-200 dark:border-zinc-700 rounded-lg">
                <table className="w-full text-sm">
                  <thead className="bg-zinc-50 dark:bg-zinc-800 text-left">
                    <tr>
                      <th className="px-3 py-2">Event type</th>
                      <th className="px-3 py-2">Endpoint</th>
                      <th className="px-3 py-2">Attempts</th>
                      <th className="px-3 py-2">Last response</th>
                      <th className="px-3 py-2">Created</th>
                      <th className="px-3 py-2">Action</th>
                    </tr>
                  </thead>
                  <tbody>
                    {deadLetters.items.map((delivery) => (
                      <tr key={delivery.id} className="border-t border-zinc-200 dark:border-zinc-700">
                        <td className="px-3 py-2">{delivery.event_type}</td>
                        <td className="px-3 py-2">
                          <div className="max-w-xs truncate" title={delivery.endpoint_url}>
                            {delivery.endpoint_url}
                          </div>
                          {!delivery.endpoint_active && (
                            <div className="text-xs text-red-600 dark:text-red-400">endpoint inactive</div>
                          )}
                        </td>
                        <td className="px-3 py-2">{delivery.attempts}</td>
                        <td className="px-3 py-2">{delivery.last_response_code ?? "—"}</td>
                        <td className="px-3 py-2 whitespace-nowrap">{new Date(delivery.created_at).toLocaleString()}</td>
                        <td className="px-3 py-2">
                          <div className="flex flex-col gap-1">
                            <form action={replayDeadLetterAction}>
                              <input type="hidden" name="id" value={delivery.id} />
                              <button
                                type="submit"
                                disabled={!delivery.endpoint_active}
                                title={
                                  delivery.endpoint_active
                                    ? "Requeue this delivery"
                                    : "Reactivate the endpoint before replaying"
                                }
                                className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded disabled:opacity-50 disabled:cursor-not-allowed"
                              >
                                Replay
                              </button>
                            </form>
                            <form action={replayEndpointAction}>
                              <input type="hidden" name="endpoint_id" value={delivery.endpoint_id} />
                              <button
                                type="submit"
                                disabled={!delivery.endpoint_active}
                                title={
                                  delivery.endpoint_active
                                    ? "Requeue every dead-letter delivery for this endpoint"
                                    : "Reactivate the endpoint before replaying"
                                }
                                className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded disabled:opacity-50 disabled:cursor-not-allowed"
                              >
                                Replay all for endpoint
                              </button>
                            </form>
                          </div>
                        </td>
                      </tr>
                    ))}
                    {deadLetters.items.length === 0 && (
                      <tr>
                        <td className="px-3 py-4 text-zinc-500" colSpan={6}>
                          No dead-letter deliveries found.
                        </td>
                      </tr>
                    )}
                  </tbody>
                </table>
              </div>

              <Pagination basePath="/operator/webhooks" page={page} perPage={PER_PAGE} total={deadLetters.total} searchParams={{}} />
            </>
          )
        )}
      </div>
    </main>
  );
}
