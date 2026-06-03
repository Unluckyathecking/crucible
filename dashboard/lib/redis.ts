import Redis from "ioredis";

declare global {
  // eslint-disable-next-line no-var
  var _crucible_redis: Redis | undefined;
}

// Returns a shared Redis client if REDIS_URL is configured, or null if not.
// The dashboard uses Redis only for best-effort cache invalidation after key
// revocation (clearing auth:{prefix} so the gateway re-checks Postgres
// immediately rather than waiting for the 60-second TTL).
//
// A single singleton is used across all invocations so that the connection is
// already established by the time revokeApiKey fires a DEL command.
// Using lazyConnect+enableOfflineQueue:false would reject the first command
// before the connection reaches "ready", silently leaving the cache entry live.
export function getRedis(): Redis | null {
  const url = process.env.REDIS_URL;
  if (!url) return null;

  if (!global._crucible_redis) {
    global._crucible_redis = new Redis(url);
  }
  return global._crucible_redis;
}
