// UUID_RE validates the lowercase UUID format PostgreSQL produces (gen_random_uuid()).
// Shared here so route.ts and db.ts use identical validation without drift.
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
