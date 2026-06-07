import { auth } from "@/auth";
import { redirect } from "next/navigation";
import Link from "next/link";
import { UsageClient } from "@/components/usage-client";

export const dynamic = "force-dynamic";

export default async function UsagePage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }

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
        <UsageClient />
      </div>
    </main>
  );
}
