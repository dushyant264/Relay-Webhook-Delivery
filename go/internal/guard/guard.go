// Package guard holds the Redis-backed protection primitives: a per-endpoint
// rate limiter and a per-endpoint circuit breaker.
//
// Redis here is a SOFT dependency: on Redis errors both primitives fail open
// (allow the call) and log, trading protection for availability. Correctness
// data never lives here.
package guard

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type Guard struct {
	rdb *redis.Client
	log *slog.Logger

	// Breaker tuning.
	FailureThreshold int           // consecutive failures that open the breaker
	OpenFor          time.Duration // how long the breaker stays open
}

func New(addr string, log *slog.Logger) *Guard {
	return &Guard{
		rdb:              redis.NewClient(&redis.Options{Addr: addr}),
		log:              log,
		FailureThreshold: 5,
		OpenFor:          2 * time.Minute,
	}
}

func (g *Guard) Ping(ctx context.Context) error { return g.rdb.Ping(ctx).Err() }
func (g *Guard) Close()                         { g.rdb.Close() }

// AllowRate implements a fixed one-second window counter per endpoint:
// INCR rl:{endpoint}:{unix-second}, allow while count <= limit. Simple, cheap,
// and accurate enough at per-endpoint scale (documented upgrade path: token
// bucket in Lua for smoother bursts).
func (g *Guard) AllowRate(ctx context.Context, endpointID string, limitPerSec int) bool {
	key := fmt.Sprintf("rl:%s:%d", endpointID, time.Now().Unix())
	pipe := g.rdb.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 2*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		g.log.Warn("rate limiter unavailable, failing open", "err", err)
		return true
	}
	return incr.Val() <= int64(limitPerSec)
}

// BreakerOpen reports whether the endpoint's circuit is currently open.
func (g *Guard) BreakerOpen(ctx context.Context, endpointID string) bool {
	n, err := g.rdb.Exists(ctx, "cb:open:"+endpointID).Result()
	if err != nil {
		g.log.Warn("breaker unavailable, failing open", "err", err)
		return false
	}
	return n > 0
}

// RecordFailure bumps the consecutive-failure counter and opens the breaker
// once the threshold is crossed. The open key's TTL is the half-open timer:
// when it expires, traffic flows again and one success resets the count.
func (g *Guard) RecordFailure(ctx context.Context, endpointID string) {
	failKey := "cb:fail:" + endpointID
	n, err := g.rdb.Incr(ctx, failKey).Result()
	if err != nil {
		g.log.Warn("breaker record-failure failed", "err", err)
		return
	}
	g.rdb.Expire(ctx, failKey, 10*time.Minute)
	if n >= int64(g.FailureThreshold) {
		g.rdb.Set(ctx, "cb:open:"+endpointID, 1, g.OpenFor)
		g.rdb.Del(ctx, failKey)
		g.log.Warn("circuit breaker OPEN", "endpoint_id", endpointID, "consecutive_failures", n, "open_for", g.OpenFor)
	}
}

// RecordSuccess resets the endpoint's failure streak.
func (g *Guard) RecordSuccess(ctx context.Context, endpointID string) {
	g.rdb.Del(ctx, "cb:fail:"+endpointID, "cb:open:"+endpointID)
}

// --- API-key cache (used by ingest) ---

// CacheGet returns a cached "applicationID:keyHash" value for an API-key prefix.
func (g *Guard) CacheGet(ctx context.Context, prefix string) (string, bool) {
	v, err := g.rdb.Get(ctx, "apikey:"+prefix).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

func (g *Guard) CacheSet(ctx context.Context, prefix, value string, ttl time.Duration) {
	g.rdb.Set(ctx, "apikey:"+prefix, value, ttl)
}
