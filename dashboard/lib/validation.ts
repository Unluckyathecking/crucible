// UUID_RE validates standard UUID format (case-insensitive). PostgreSQL's
// gen_random_uuid() always produces lowercase, but URL parameters and external
// callers may use uppercase hex digits; the /i flag accepts both without drift.
// Shared here so route.ts and db.ts use identical validation without drift.
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// KEY_NAME_RE validates API key name input. Shared between POST /api/keys (route.ts)
// and CreateKeyForm (create-key-form.tsx) so both server and client enforce the same
// character set without independent copies drifting apart over time.
export const KEY_NAME_RE = /^[a-zA-Z0-9 _\-./]+$/;
