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
// enableOfflineQueue is left at its ioredis default (true): DELs issued on
// a cold process before the TCP handshake completes are queued and sent once
// the connection reaches "ready", rather than being rejected immediately.
// In practice the queue drains within milliseconds of process start; the only
// commands that accumulate are those issued during that brief window.
export function getRedis(): Redis | null {
  const url = process.env.REDIS_URL;
  if (!url) return null;

  if (!global._crucible_redis) {
    const redis = new Redis(url, {
      maxRetriesPerRequest: 2,
    });
    redis.on("error", (err) => {
      console.error("Redis client error:", err.message);
    });
    global._crucible_redis = redis;
  }
  return global._crucible_redis;
}
