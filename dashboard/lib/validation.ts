// UUID_RE validates standard UUID format (v1–v5) case-insensitively.
// Shared here so route.ts and db.ts use identical validation without drift.
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
