// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.
//
// SERVER-ONLY. Imports lib/license (Node crypto) and reads process.env. Never
// import from auth.config.ts (Edge runtime) or any client component. The login
// page and auth.ts are the only intended importers.
import type { OIDCConfig } from "next-auth/providers";
import { loadLicense, hasFeature, FEATURE_SSO } from "@/lib/license";

// The generic-OIDC provider id and the value the login form posts to signIn().
export const SSO_PROVIDER_ID = "sso";

// ssoStatus is the single source of truth for whether SSO is enabled. SSO is on
// iff: a valid license grants the `sso` feature AND all three OIDC env vars are
// set. Anything short of that -> disabled, and the dashboard behaves exactly as
// the community edition (magic-link only). Returns ONLY a boolean + display name
// so callers can safely pass the result to a client component — the license key
// and its parsed contents never leave the server.
export function ssoStatus(): { enabled: boolean; displayName: string } {
  const displayName = process.env.SSO_DISPLAY_NAME?.trim() || "SSO";
  const issuer = process.env.SSO_OIDC_ISSUER?.trim();
  const clientId = process.env.SSO_OIDC_CLIENT_ID?.trim();
  const clientSecret = process.env.SSO_OIDC_CLIENT_SECRET?.trim();
  if (!issuer || !clientId || !clientSecret) {
    return { enabled: false, displayName };
  }
  const enabled = hasFeature(loadLicense(), FEATURE_SSO);
  return { enabled, displayName };
}

// ssoProvider builds the generic OIDC provider for NextAuth, or returns null when
// SSO is disabled. Kept out of auth.config.ts on purpose: it depends on the
// license check (Node crypto), which must never load on the Edge runtime.
export function ssoProvider(): OIDCConfig<Record<string, unknown>> | null {
  const { enabled, displayName } = ssoStatus();
  if (!enabled) return null;

  return {
    id: SSO_PROVIDER_ID,
    name: displayName,
    type: "oidc",
    issuer: process.env.SSO_OIDC_ISSUER!.trim(),
    clientId: process.env.SSO_OIDC_CLIENT_ID!.trim(),
    clientSecret: process.env.SSO_OIDC_CLIENT_SECRET!.trim(),
    // WHY allowDangerousEmailAccountLinking: a customer who first signed in via
    // magic-link already has a users row keyed by their email. Without linking,
    // an OIDC sign-in with that same email would trip NextAuth's
    // OAuthAccountNotLinked guard. Linking by email is safe here ONLY because a
    // conforming OIDC provider returns email_verified=true (the IdP has verified
    // the address); we require operators to point SSO_OIDC_ISSUER at such an IdP.
    // Customer identity downstream is keyed on email via ensureCustomer(), so
    // linking cannot create a duplicate customer regardless.
    allowDangerousEmailAccountLinking: true,
  };
}
