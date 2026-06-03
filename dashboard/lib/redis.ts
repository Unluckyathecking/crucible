import Redis from "ioredis";

declare global {
  // eslint-disable-next-line no-var
  var _crucible_redis: Redis | undefined;
}

// Returns a Redis client if REDIS_URL is configured, or null if not.
// The dashboard uses Redis only for best-effort cache invalidation after key
// revocation (clearing auth:{prefix} so the gateway re-checks Postgres
// immediately rather than waiting for the 60-second TTL).
export function getRedis(): Redis | null {
  const url = process.env.REDIS_URL;
  if (!url) return null;

  if (process.env.NODE_ENV !== "production") {
    if (!global._crucible_redis) {
      global._crucible_redis = new Redis(url, { lazyConnect: true, enableOfflineQueue: false });
    }
    return global._crucible_redis;
  }
  return new Redis(url, { lazyConnect: true, enableOfflineQueue: false });
}
