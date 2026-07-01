import Link from "next/link";
import { OperatorNav } from "./_components/operator-nav";

export const dynamic = "force-dynamic";

export default function OperatorHomePage() {
  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-5xl">
        <OperatorNav />
        <h1 className="text-2xl sm:text-3xl font-bold mb-6">Operator console</h1>
        <p className="text-sm text-zinc-600 dark:text-zinc-400 mb-6">
          Read-only view over the gateway&apos;s admin API. No writes are available from this console.
        </p>
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Link
            href="/operator/customers"
            className="block border border-zinc-200 dark:border-zinc-700 rounded-lg p-4 hover:border-zinc-400 dark:hover:border-zinc-500"
          >
            <h2 className="font-semibold mb-1">Customers</h2>
            <p className="text-sm text-zinc-600 dark:text-zinc-400">Browse customers, filter by plan.</p>
          </Link>
          <Link
            href="/operator/audit"
            className="block border border-zinc-200 dark:border-zinc-700 rounded-lg p-4 hover:border-zinc-400 dark:hover:border-zinc-500"
          >
            <h2 className="font-semibold mb-1">Audit log</h2>
            <p className="text-sm text-zinc-600 dark:text-zinc-400">Search audit events by customer, action, or date.</p>
          </Link>
          <Link
            href="/operator/plans"
            className="block border border-zinc-200 dark:border-zinc-700 rounded-lg p-4 hover:border-zinc-400 dark:hover:border-zinc-500"
          >
            <h2 className="font-semibold mb-1">Plans</h2>
            <p className="text-sm text-zinc-600 dark:text-zinc-400">View configured plan tiers.</p>
          </Link>
        </div>
      </div>
    </main>
  );
}
