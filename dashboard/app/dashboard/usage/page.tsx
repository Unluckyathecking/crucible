import { auth } from "@/auth";
import { redirect } from "next/navigation";
import Link from "next/link";
import { UsageClient } from "@/components/usage-client";
import { toISODateString, MS_PER_DAY } from "@/lib/usage-format";

export const dynamic = "force-dynamic";

const USAGE_WINDOW_DAYS = 30;

export default async function UsagePage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }

  // Compute initial date range server-side for the SSR → hydrate path.
  // With force-dynamic this runs per-request on the server; client-side
  // navigation re-renders UsageClient with cached props until a full reload.
  const now = new Date();
  const todayUTC = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const tomorrowUTC = new Date(todayUTC.getTime() + MS_PER_DAY);
  // today − (USAGE_WINDOW_DAYS − 1) gives an inclusive window of exactly
  // USAGE_WINDOW_DAYS days: [today−29, today] = 30 calendar days inclusive.
  const initialFrom = toISODateString(new Date(todayUTC.getTime() - (USAGE_WINDOW_DAYS - 1) * MS_PER_DAY));
  const initialTo = toISODateString(todayUTC);
  const initialApiTo = toISODateString(tomorrowUTC);

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-3xl">
        <header className="flex items-center gap-4 mb-6 sm:mb-8">
          <Link
            href="/dashboard"
            className="text-sm text-zinc-500 hover:text-zinc-900"
          >
            ← Dashboard
          </Link>
          <h1 className="text-2xl sm:text-3xl font-bold">Usage Analytics</h1>
        </header>
        <UsageClient initialFrom={initialFrom} initialTo={initialTo} initialApiTo={initialApiTo} />
      </div>
    </main>
  );
}
