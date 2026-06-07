"use client";

import { useState } from "react";
import Link from "next/link";

interface BillingPageContentProps {
  planId: string;
  hasStripeCustomer: boolean;
}

function UpgradeButton() {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleUpgrade() {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch("/api/billing/checkout", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Requested-With": "XMLHttpRequest",
        },
        body: JSON.stringify({ plan_id: "pro" }),
      });
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: { message?: string } };
        setError(body.error?.message ?? "Upgrade failed. Please try again.");
        return;
      }
      const { url } = (await res.json()) as { url: string };
      window.location.href = url;
    } catch {
      setError("Network error. Please try again.");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div>
      <button
        onClick={() => void handleUpgrade()}
        disabled={loading}
        className="inline-flex items-center px-4 py-2 bg-zinc-900 text-white text-sm font-medium rounded-md hover:bg-zinc-700 disabled:opacity-50 disabled:cursor-not-allowed"
      >
        {loading ? "Loading…" : "Upgrade plan"}
      </button>
      {error && <p className="mt-2 text-sm text-red-600">{error}</p>}
    </div>
  );
}

function ManageBillingButton() {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleManage() {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch("/api/billing/portal", {
        method: "POST",
        headers: { "X-Requested-With": "XMLHttpRequest" },
      });
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: { message?: string } };
        setError(body.error?.message ?? "Could not open billing portal. Please try again.");
        return;
      }
      const { url } = (await res.json()) as { url: string };
      window.location.href = url;
    } catch {
      setError("Network error. Please try again.");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div>
      <button
        onClick={() => void handleManage()}
        disabled={loading}
        className="inline-flex items-center px-4 py-2 border border-zinc-300 text-sm font-medium rounded-md hover:bg-zinc-50 disabled:opacity-50 disabled:cursor-not-allowed"
      >
        {loading ? "Loading…" : "Manage billing"}
      </button>
      {error && <p className="mt-2 text-sm text-red-600">{error}</p>}
    </div>
  );
}

export default function BillingPageContent({ planId, hasStripeCustomer }: BillingPageContentProps) {
  const isFree = planId === "free";

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-3xl">
        <header className="flex items-center gap-3 mb-6 sm:mb-8">
          <Link href="/dashboard" className="text-sm text-zinc-500 hover:text-zinc-900 underline">
            ← Dashboard
          </Link>
          <h1 className="text-2xl sm:text-3xl font-bold">Billing</h1>
        </header>

        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5 mb-5 sm:mb-6">
          <h2 className="text-lg sm:text-xl font-semibold mb-3">Current plan</h2>
          <div className="text-sm text-zinc-500">Plan</div>
          <div className="text-base sm:text-lg font-medium uppercase mb-4">{planId}</div>

          {isFree ? (
            <div>
              <p className="text-sm text-zinc-600 mb-4">
                You are on the free tier. Upgrade to unlock higher rate limits and usage caps.
              </p>
              <UpgradeButton />
            </div>
          ) : (
            <div>
              <p className="text-sm text-zinc-600 mb-4">
                You are on a paid plan. Manage your subscription, update payment methods, or download invoices.
              </p>
              {hasStripeCustomer ? (
                <ManageBillingButton />
              ) : (
                <p className="text-sm text-zinc-500 italic">
                  Billing portal not yet available — your subscription is being set up.
                </p>
              )}
            </div>
          )}
        </section>
      </div>
    </main>
  );
}
