"use client";

import React, { useState } from "react";

export function WebhooksFormClient() {
  const [url, setUrl] = useState("");
  const [status, setStatus] = useState<
    | null
    | { type: "success"; secret: string }
    | { type: "error"; msg: string }
  >(null);
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setStatus(null);
    try {
      const res = await fetch("/api/webhooks", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "x-requested-with": "xmlhttprequest",
        },
        body: JSON.stringify({ url }),
      });
      if (!res.ok) {
        const text = await res.text();
        setStatus({ type: "error", msg: text || `Error ${res.status}` });
        return;
      }
      const data = (await res.json()) as { secret_hex: string };
      setStatus({ type: "success", secret: data.secret_hex });
      setUrl("");
    } catch (err) {
      setStatus({
        type: "error",
        msg: err instanceof Error ? err.message : "Request failed",
      });
    } finally {
      setLoading(false);
    }
  }

  return (
    <div>
      <form onSubmit={handleSubmit} className="flex flex-col sm:flex-row gap-2">
        <input
          type="url"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://your-server.example.com/webhook"
          required
          disabled={loading}
          className="flex-1 rounded border border-zinc-300 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-zinc-400 disabled:opacity-50"
        />
        <button
          type="submit"
          disabled={loading || !url}
          className="rounded bg-zinc-900 px-4 py-2 text-sm font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
        >
          {loading ? "Adding…" : "Add endpoint"}
        </button>
      </form>
      {status?.type === "success" && (
        <div className="mt-3 rounded-lg border border-green-200 bg-green-50 p-3">
          <p className="text-sm font-medium text-green-800 mb-1">
            Endpoint registered. Copy your signing secret — it will not be
            shown again.
          </p>
          <code className="block font-mono text-xs bg-white border border-green-200 rounded px-2 py-1 break-all text-green-900">
            {status.secret}
          </code>
        </div>
      )}
      {status?.type === "error" && (
        <p className="mt-2 text-sm text-red-600">{status.msg}</p>
      )}
    </div>
  );
}

export function RevokeEndpointButton({ endpointId }: { endpointId: string }) {
  const [loading, setLoading] = useState(false);
  const [revoked, setRevoked] = useState(false);

  async function handleRevoke() {
    if (
      !confirm("Revoke this endpoint? Deliveries to it will stop immediately.")
    )
      return;
    setLoading(true);
    try {
      const res = await fetch(`/api/webhooks/${endpointId}`, {
        method: "DELETE",
        headers: { "x-requested-with": "xmlhttprequest" },
      });
      if (res.ok) {
        setRevoked(true);
      } else {
        alert(`Failed to revoke endpoint: HTTP ${res.status}`);
      }
    } finally {
      setLoading(false);
    }
  }

  if (revoked) return <span className="text-xs text-zinc-400">Revoked</span>;
  return (
    <button
      onClick={handleRevoke}
      disabled={loading}
      className="shrink-0 rounded border border-red-200 px-3 py-1 text-xs text-red-600 hover:bg-red-50 disabled:opacity-50"
    >
      {loading ? "…" : "Revoke"}
    </button>
  );
}
