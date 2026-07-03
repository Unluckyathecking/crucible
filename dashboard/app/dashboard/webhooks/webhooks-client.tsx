"use client";

import React, { useState } from "react";
import { useRouter } from "next/navigation";

/**
 * EventTypeCheckboxes renders an "All events" toggle plus a per-type checkbox
 * list, shared by the create form and the edit-subscription control so both
 * present the same selection UX. selected === null means "all events".
 */
function EventTypeCheckboxes({
  eventTypes,
  selected,
  onChange,
  disabled,
}: {
  eventTypes: readonly string[];
  selected: string[] | null;
  onChange: (next: string[] | null) => void;
  disabled?: boolean;
}) {
  const allEvents = selected === null;

  return (
    <div className="flex flex-col gap-1.5">
      <label className="flex items-center gap-2 text-xs text-zinc-600">
        <input
          type="checkbox"
          checked={allEvents}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked ? null : [])}
        />
        All events
      </label>
      {!allEvents && (
        <div className="ml-5 flex flex-col gap-1">
          {eventTypes.map((t) => (
            <label key={t} className="flex items-center gap-2 text-xs text-zinc-600">
              <input
                type="checkbox"
                checked={selected.includes(t)}
                disabled={disabled}
                onChange={(e) =>
                  onChange(
                    e.target.checked
                      ? [...selected, t]
                      : selected.filter((s) => s !== t),
                  )
                }
              />
              <code className="font-mono">{t}</code>
            </label>
          ))}
        </div>
      )}
    </div>
  );
}

export function WebhooksFormClient({ eventTypes }: { eventTypes: readonly string[] }) {
  const [url, setUrl] = useState("");
  const [subscribedEvents, setSubscribedEvents] = useState<string[] | null>(null);
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
        body: JSON.stringify({ url, subscribed_events: subscribedEvents }),
      });
      if (!res.ok) {
        const text = await res.text();
        setStatus({ type: "error", msg: text || `Error ${res.status}` });
        return;
      }
      const data: unknown = await res.json();
      if (
        typeof data !== "object" ||
        data === null ||
        typeof (data as Record<string, unknown>).secret_hex !== "string"
      ) {
        setStatus({ type: "error", msg: "Invalid response from server" });
        return;
      }
      setStatus({
        type: "success",
        secret: (data as Record<string, unknown>).secret_hex as string,
      });
      setUrl("");
      setSubscribedEvents(null);
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
      <form onSubmit={handleSubmit} className="flex flex-col gap-3">
        <div className="flex flex-col sm:flex-row gap-2">
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
        </div>
        <EventTypeCheckboxes
          eventTypes={eventTypes}
          selected={subscribedEvents}
          onChange={setSubscribedEvents}
          disabled={loading}
        />
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

export function EditSubscriptionButton({
  endpointId,
  eventTypes,
  current,
}: {
  endpointId: string;
  eventTypes: readonly string[];
  current: string[] | null;
}) {
  const router = useRouter();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<string[] | null>(current);
  const [saved, setSaved] = useState<string[] | null>(current);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSave() {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`/api/webhooks/${endpointId}`, {
        method: "PATCH",
        headers: {
          "content-type": "application/json",
          "x-requested-with": "xmlhttprequest",
        },
        body: JSON.stringify({ subscribed_events: draft }),
      });
      if (!res.ok) {
        const text = await res.text();
        setError(text || `Failed to update subscription: HTTP ${res.status}`);
        return;
      }
      setSaved(draft);
      setEditing(false);
      // The "Subscribed:" text next to this button is rendered by the server
      // page from its own ep.subscribed_events prop, not from this component's
      // state — refresh so it picks up the change instead of showing stale data.
      router.refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Network error");
    } finally {
      setLoading(false);
    }
  }

  if (!editing) {
    return (
      <button
        onClick={() => {
          setDraft(saved);
          setEditing(true);
        }}
        className="rounded border border-zinc-200 px-3 py-1 text-xs text-zinc-600 hover:bg-zinc-50"
      >
        Edit subscription
      </button>
    );
  }

  return (
    <div className="flex flex-col items-end gap-2 rounded border border-zinc-200 p-2">
      <EventTypeCheckboxes
        eventTypes={eventTypes}
        selected={draft}
        onChange={setDraft}
        disabled={loading}
      />
      <div className="flex gap-2">
        <button
          onClick={() => setEditing(false)}
          disabled={loading}
          className="rounded border border-zinc-200 px-3 py-1 text-xs text-zinc-600 hover:bg-zinc-50 disabled:opacity-50"
        >
          Cancel
        </button>
        <button
          onClick={handleSave}
          disabled={loading}
          className="rounded bg-zinc-900 px-3 py-1 text-xs font-medium text-white hover:bg-zinc-700 disabled:opacity-50"
        >
          {loading ? "Saving…" : "Save"}
        </button>
      </div>
      {error && <p className="text-xs text-red-600">{error}</p>}
    </div>
  );
}

export function RevokeEndpointButton({ endpointId }: { endpointId: string }) {
  const [loading, setLoading] = useState(false);
  const [revoked, setRevoked] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleRevoke() {
    if (
      !confirm("Revoke this endpoint? Deliveries to it will stop immediately.")
    )
      return;
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`/api/webhooks/${endpointId}`, {
        method: "DELETE",
        headers: { "x-requested-with": "xmlhttprequest" },
      });
      if (res.ok) {
        setRevoked(true);
      } else {
        setError(`Failed to revoke endpoint: HTTP ${res.status}`);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Network error");
    } finally {
      setLoading(false);
    }
  }

  if (revoked) return <span className="text-xs text-zinc-400">Revoked</span>;
  return (
    <div className="shrink-0 flex flex-col items-end gap-1">
      <button
        onClick={handleRevoke}
        disabled={loading}
        className="rounded border border-red-200 px-3 py-1 text-xs text-red-600 hover:bg-red-50 disabled:opacity-50"
      >
        {loading ? "…" : "Revoke"}
      </button>
      {error && <p className="text-xs text-red-600">{error}</p>}
    </div>
  );
}
