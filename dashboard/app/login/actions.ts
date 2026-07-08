"use server";

import { signIn } from "@/auth";
import { SSO_PROVIDER_ID } from "@/lib/sso";

export async function signInWithEmail(formData: FormData) {
  await signIn("nodemailer", formData);
}

// signInWithSSO kicks off the OIDC redirect flow. The provider is only registered
// in auth.ts when SSO is licensed + configured, so this is unreachable (and would
// no-op/error) in community mode — the button that posts here is not rendered
// unless ssoStatus().enabled is true.
export async function signInWithSSO() {
  await signIn(SSO_PROVIDER_ID, { redirectTo: "/dashboard" });
}
