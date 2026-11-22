// Author: Patrick Munis at newwave
// tenant_registry.go — provisioned-tenant registry behind the tenancy
// middleware. A credential whose group claim names a tenant with no
// provisioned schema (e.g. /tenant/zz) used to sail through membership and
// die at the query with a generic 500 (DAST finding F2, 2026-11-22). The
// middleware now asks this registry first and fails closed with a precise
// 503 before any handler runs.
//
// The checker caches information_schema lookups (60s TTL) so the hot path
// costs one map read; a freshly provisioned tenant becomes visible within a
// minute without a restart. DB errors are treated as "unknown", and unknown
// fails closed the same way as missing — a wrong answer here either leaks a
// confusing 500 (too open) or briefly 503s a healthy tenant (too closed),
// and closed is the safe side.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tenantExists answers "is tenant <st> provisioned?". Replaced by
// installTenantRegistry at startup; the default reports everything as
// provisioned so unit tests and pre-DB paths keep their historical behavior.
var tenantExists = func(string) bool { return true }

type tenantCacheEntry struct {
	exists    bool
	fetchedAt time.Time
}

// installTenantRegistry points tenantExists at a cached lookup over
// information_schema.schemata. Called once the pool exists, before routes
// serve.
func installTenantRegistry(pool *pgxpool.Pool) {
	const ttl = 60 * time.Second
	var mu sync.Mutex
	cache := map[string]tenantCacheEntry{}
	tenantExists = func(tenant string) bool {
		mu.Lock()
		e, ok := cache[tenant]
		if ok && time.Since(e.fetchedAt) < ttl {
			mu.Unlock()
			return e.exists
		}
		mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.schemata
			                WHERE schema_name = 'tenant_' || $1)`, tenant).Scan(&exists)
		if err != nil {
			return false // unknown == closed (see file header)
		}

		mu.Lock()
		cache[tenant] = tenantCacheEntry{exists: exists, fetchedAt: time.Now()}
		mu.Unlock()
		return exists
	}
}
