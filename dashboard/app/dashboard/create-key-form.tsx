"use client";

import { useActionState } from "react";
import { useRouter } from "next/navigation";

interface CreateKeyFormProps {
  existingNames: string[];
}

type State = { error: string | null; submitted: boolean; key: string | null };

export function CreateKeyForm({ existingNames }: CreateKeyFormProps) {
  const router = useRouter();
  const [state, formAction, isPending] = useActionState(
    async (_prev: State, formData: FormData): Promise<State> => {
      const name = (formData.get("name") as string)?.trim() ?? "";

      if (!name) {
        return { error: "Name is required", submitted: false, key: null };
      }
      if (name.length < 2) {
        return { error: "Name must be at least 2 characters", submitted: false, key: null };
      }
      if (name.length > 64) {
        return { error: "Name must be 64 characters or fewer", submitted: false, key: null };
      }
      if (!/^[a-zA-Z0-9 _\-./]+$/.test(name)) {
        return {
          error: "Name can only contain letters, numbers, spaces, hyphens, underscores, dots, and slashes",
          submitted: false,
          key: null,
        };
      }
      if (existingNames.includes(name)) {
        return { error: "A key with this name already exists", submitted: false, key: null };
      }

      try {
        const res = await fetch("/api/keys", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ name }),
        });

        if (!res.ok) {
          const text = await res.text();
          return { error: text, submitted: false, key: null };
        }

        const data = (await res.json()) as { key?: unknown };
        if (typeof data.key !== "string") {
          return { error: "Invalid response from server", submitted: false, key: null };
        }
        await router.refresh();
        return { error: null, submitted: true, key: data.key };
      } catch {
        return { error: "Network error. Try again.", submitted: false, key: null };
      }
    },
    { error: null, submitted: false, key: null },
  );

  return (
    <form action={formAction} className="flex flex-col gap-2" noValidate>
      <div className="flex flex-col sm:flex-row gap-2 sm:items-end">
        <div className="flex-1 min-w-0">
          <label htmlFor="key-name" className="sr-only">
            Key name
          </label>
          <input
            id="key-name"
            type="text"
            name="name"
            placeholder="e.g. production, staging, local-dev"
            required
            minLength={2}
            maxLength={64}
            pattern="[a-zA-Z0-9 _\-./]+"
            aria-describedby={state.error ? "key-name-error" : undefined}
            aria-invalid={!!state.error}
            className="w-full px-3 py-2 text-sm border border-zinc-300 rounded bg-transparent placeholder:text-zinc-400 focus:outline-none focus:ring-2 focus:ring-zinc-900 focus:border-transparent disabled:opacity-50"
            disabled={isPending}
          />
          {state.error && (
            <p id="key-name-error" className="mt-1 text-xs text-red-600" role="alert">
              {state.error}
            </p>
          )}
        </div>
        <button
          type="submit"
          disabled={isPending}
          className="px-4 py-2 text-sm bg-zinc-900 text-white rounded hover:bg-zinc-700 transition disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap"
        >
          {isPending ? "Creating…" : "Create key"}
        </button>
      </div>
      {state.submitted && state.key && (
        <div className="p-3 bg-emerald-50 border border-emerald-200 rounded space-y-1">
          <p className="text-xs text-emerald-700 font-medium" role="status">
            Key created — copy it now, it won&apos;t be shown again.
          </p>
          <code className="block text-xs font-mono bg-white border border-emerald-200 px-2 py-1 rounded break-all select-all">
            {state.key}
          </code>
        </div>
      )}
    </form>
  );
}

interface RevokeKeyButtonProps {
  keyId: string;
  keyPrefix: string;
}

type RevokeState = { error: string | null };

export function RevokeKeyButton({ keyId, keyPrefix }: RevokeKeyButtonProps) {
  const router = useRouter();
  const [state, formAction, isPending] = useActionState(
    async (_prev: RevokeState, _formData: FormData): Promise<RevokeState> => {
      try {
        const res = await fetch(`/api/keys/${keyId}`, {
          method: "DELETE",
          headers: { "X-Requested-With": "XMLHttpRequest" },
        });
        if (!res.ok) {
          const text = await res.text();
          return { error: text || "Failed to revoke key" };
        }
        await router.refresh();
        return { error: null };
      } catch (err) {
        console.error("RevokeKeyButton fetch failed:", err instanceof Error ? err.message : String(err));
        return { error: "Network error. Try again." };
      }
    },
    { error: null },
  );

  return (
    <div className="inline-flex flex-col items-start gap-0.5">
      <form
        action={formAction}
        onSubmit={(e) => {
          if (!window.confirm(`Revoke key ${keyPrefix}? This cannot be undone.`)) {
            e.preventDefault();
          }
        }}
      >
        <button
          type="submit"
          disabled={isPending}
          aria-label={`Revoke key ${keyPrefix}`}
          className="px-2 py-0.5 text-xs text-red-600 border border-red-300 rounded hover:bg-red-50 transition disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {isPending ? "Revoking…" : "Revoke"}
        </button>
      </form>
      {state.error && (
        <p className="text-xs text-red-600" role="alert">
          {state.error}
        </p>
      )}
    </div>
  );
}
