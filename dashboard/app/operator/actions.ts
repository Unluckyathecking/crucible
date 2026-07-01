"use server";

import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { ALLOWED_ORIGIN } from "@/lib/env";
import { OPERATOR_SESSION_COOKIE, constantTimeTokenEquals, createOperatorSessionCookie } from "@/lib/operator/session";

export async function operatorLogin(formData: FormData): Promise<void> {
  const candidate = (formData.get("token") as string | null)?.trim() ?? "";
  const expected = process.env.OPERATOR_TOKEN;

  if (!expected || expected.length < 32 || !candidate || !(await constantTimeTokenEquals(candidate, expected))) {
    redirect("/operator/login?error=1");
  }

  const session = await createOperatorSessionCookie();
  const store = await cookies();
  store.set(session.name, session.value, {
    httpOnly: true,
    sameSite: "strict",
    // Matches the __csrf cookie's check in middleware.ts: NODE_ENV alone misses
    // TLS-terminating proxies/staging where the origin is https but NODE_ENV
    // isn't exactly "production".
    secure: process.env.NODE_ENV === "production" || ALLOWED_ORIGIN.startsWith("https://"),
    path: "/",
    maxAge: session.maxAge,
  });
  redirect("/operator");
}

export async function operatorLogout(): Promise<void> {
  const store = await cookies();
  // Explicit path: "/" — must match the path the cookie was set with (login,
  // above) so the expiring Set-Cookie actually overwrites it regardless of
  // which nested /operator/* route the sign-out form was submitted from.
  store.delete({ name: OPERATOR_SESSION_COOKIE, path: "/" });
  redirect("/operator/login");
}
