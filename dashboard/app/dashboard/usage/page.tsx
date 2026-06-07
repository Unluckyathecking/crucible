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

  // Compute initial date range server-side so the client component receives
  // stable string props. This eliminates any risk of SSR/hydration mismatch
  // from calling new Date() independently on server and client.
  const now = new Date();
  const todayUTC = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const tomorrowUTC = new Date(todayUTC.getTime() + MS_PER_DAY);
  const initialFrom = toISODateString(new Date(tomorrowUTC.getTime() - USAGE_WINDOW_DAYS * MS_PER_DAY));
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
