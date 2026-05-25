"use client";

import { useFormStatus } from "react-dom";
import type { JSX } from "react";

export function SubmitButton(): JSX.Element {
  const { pending } = useFormStatus();

  return (
    <button
      type="submit"
      disabled={pending}
      aria-busy={pending}
      className="w-full px-3 py-2 bg-zinc-900 dark:bg-zinc-100 text-white dark:text-zinc-900 rounded hover:bg-zinc-700 dark:hover:bg-zinc-200 transition disabled:opacity-50 disabled:cursor-not-allowed"
    >
      {pending ? "Sending..." : "Send magic link"}
    </button>
  );
}
