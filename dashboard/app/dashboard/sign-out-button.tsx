"use client";

import { useFormStatus } from "react-dom";

export function SignOutButton() {
  const { pending } = useFormStatus();

  return (
    <button
      type="submit"
      disabled={pending}
      aria-busy={pending}
      className="text-sm text-zinc-500 hover:underline disabled:opacity-50 disabled:no-underline disabled:cursor-not-allowed"
    >
      {pending ? "Signing out..." : "Sign out"}
    </button>
  );
}
