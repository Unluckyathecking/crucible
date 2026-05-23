"use client";

import { useActionState } from "react";

interface CreateKeyFormProps {
  existingNames: string[];
}

async function createKeyAction(_prev: State, formData: FormData): Promise<State> {
  const name = (formData.get("name") as string)?.trim() ?? "";

  if (!name) {
    return { error: "Name is required", submitted: false };
  }
  if (name.length < 2) {
    return { error: "Name must be at least 2 characters", submitted: false };
  }
  if (name.length > 64) {
    return { error: "Name must be 64 characters or fewer", submitted: false };
  }
  if (!/^[a-zA-Z0-9 _\-./]+$/.test(name)) {
    return { error: "Name can only contain letters, numbers, spaces, hyphens, underscores, dots, and slashes", submitted: false };
  }

  try {
    const res = await fetch("/api/keys", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name }),
    });

    if (!res.ok) {
      const text = await res.text();
      return { error: text, submitted: false };
    }

    const html = await res.text();
    // Open the key reveal page in a new tab so the dashboard stays intact
    const blob = new Blob([html], { type: "text/html" });
    const url = URL.createObjectURL(blob);
    window.open(url, "_blank");
    return { error: null, submitted: true };
  } catch {
    return { error: "Network error. Try again.", submitted: false };
  }
}

type State = { error: string | null; submitted: boolean };

export function CreateKeyForm({ existingNames }: CreateKeyFormProps) {
  const [state, formAction, isPending] = useActionState(createKeyAction, {
    error: null,
    submitted: false,
  });

  return (
    <form action={formAction} className="flex flex-col sm:flex-row gap-2 sm:items-end" noValidate>
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
        {state.submitted && (
          <p className="mt-1 text-xs text-emerald-600" role="status">
            Key created — check the new tab.
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
    </form>
  );
}
