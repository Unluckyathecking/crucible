import { auth } from "@/auth";
import { redirect } from "next/navigation";
import Link from "next/link";
import { ErrorsClient } from "./errors-client";

export const dynamic = "force-dynamic";

const ERRORS_WINDOW_DAYS = 30;
const MS_PER_DAY = 86_400_000;

function toISODate(d: Date): string {
  return d.toISOString().slice(0, 10);
}

export default async function ErrorsPage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }

  const now = new Date();
  const todayUTC = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const tomorrowUTC = new Date(todayUTC.getTime() + MS_PER_DAY);
  // today − (ERRORS_WINDOW_DAYS − 1) gives an inclusive window of exactly
  // ERRORS_WINDOW_DAYS calendar days: [today−29, today] = 30 days inclusive.
  const initialFrom = toISODate(new Date(todayUTC.getTime() - (ERRORS_WINDOW_DAYS - 1) * MS_PER_DAY));
  const initialTo = toISODate(todayUTC);       // display-inclusive upper bound
  const initialApiTo = toISODate(tomorrowUTC); // exclusive upper bound for the API call

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-5xl">
        <header className="flex items-center gap-4 mb-6 sm:mb-8">
          <Link
            href="/dashboard"
            className="text-sm text-zinc-500 hover:text-zinc-900"
          >
            ← Dashboard
          </Link>
          <h1 className="text-2xl sm:text-3xl font-bold">Error History</h1>
        </header>
        <ErrorsClient
          initialFrom={initialFrom}
          initialTo={initialTo}
          initialApiTo={initialApiTo}
        />
      </div>
    </main>
  );
}
