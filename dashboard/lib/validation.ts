// UUID_RE validates standard UUID format (case-insensitive). PostgreSQL's
// gen_random_uuid() always produces lowercase, but URL parameters and external
// callers may use uppercase hex digits; the /i flag accepts both without drift.
// Shared here so route.ts and db.ts use identical validation without drift.
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
