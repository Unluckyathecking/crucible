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
// A global singleton is used so the connection is established once and reused;
// creating a new client per call would leak TCP connections and ensure the
// first DEL always fails before the connection reaches "ready".
//
// The offline queue is enabled (ioredis default) so DELs issued on a cold
// process — before the TCP handshake completes — are queued and sent once
// ready rather than rejected. maxOfflineQueue:50 caps memory if Redis is
// persistently unreachable; commands beyond the cap are dropped, which is
// acceptable for best-effort invalidation (the 60 s TTL is the fallback).
export function getRedis(): Redis | null {
  const url = process.env.REDIS_URL;
  if (!url) return null;

  if (!global._crucible_redis) {
    const redis = new Redis(url, {
      maxRetriesPerRequest: 2,
      maxOfflineQueue: 50,
    });
    redis.on("error", (err) => {
      console.error("Redis client error:", err.message);
    });
    global._crucible_redis = redis;
  }
  return global._crucible_redis;
}
