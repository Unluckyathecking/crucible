"use server";

import { cookies } from "next/headers";
import { redirect } from "next/navigation";
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
    secure: process.env.NODE_ENV === "production",
    path: "/",
    maxAge: session.maxAge,
  });
  redirect("/operator");
}

export async function operatorLogout(): Promise<void> {
  const store = await cookies();
  store.delete(OPERATOR_SESSION_COOKIE);
  redirect("/operator/login");
}
