# redis-cache-mock-api

A mock user API demonstrating the cache-aside pattern backed by Redis, sitting in front of a real Postgres database.

**Live:** https://redis-cache-mock-api.onrender.com
**CI:** tested by [go-api-cache-mock-tests](https://github.com/kennethchuaqiyang/go-api-cache-mock-tests) and [playwright-api-cache-mock-tests](https://github.com/kennethchuaqiyang/playwright-api-cache-mock-tests)

## What this demonstrates

Redis provides **active/server-side TTL expiration** — a cache key is removed by Redis itself once its TTL elapses, with no read required to trigger cleanup. This is the key behavioral contrast with the [in-memory](https://github.com/kennethchuaqiyang/inmemory-cache-mock-api) and [ELK](https://github.com/kennethchuaqiyang/elk-cache-mock-api) variants of this same API, which both rely on lazy expiration instead.

## Endpoints

### `GET /api/user?user_id={id}`

Cache-aside read:
- **Cache miss** (first read, or after invalidation): reads Postgres, populates Redis with a TTL, responds
- **Cache hit** (within TTL): served directly from Redis, no DB hit

Response headers:
| Header | Meaning |
|---|---|
| `X-Cache` | `HIT` or `MISS` |
| `X-User-Id` | echoes the requested user_id |
| `X-Browser` | echoes the request's `X-Browser` header (or `User-Agent` if absent) |
| `Set-Cookie: cache_key=...` | the Redis key, with `Max-Age` set to the remaining TTL |

Response body:
```json
{"user_id": 2, "username": "mary", "location": "Kuala Lumpur", "salary": 8000}
```

### `PUT /api/user/update`

Body: `{"user_id": 2, "salary": 9000}`

- New salary equals current DB value → `{"message": "No update"}`, cache untouched
- DB write fails → `{"message": "No Success"}`
- DB write succeeds → Redis key deleted (`DEL`), `{"message": "Success"}`

## Environment variables

| Var | Purpose | Default |
|---|---|---|
| `DATABASE_URL` | Postgres connection string (Neon) | required |
| `REDIS_URL` | Redis connection string (`rediss://...` for TLS) | required |
| `PORT` | HTTP port | `8080` |
| `CACHE_TTL_SECONDS` | cache TTL | `30` |

## Running locally

```bash
docker compose up -d redis   # from the parent cache-mock-servers/ folder
go mod tidy
go run main.go
```

## Deployment

Deployed on Render (Oregon, free tier), with [Upstash](https://upstash.com) providing the managed Redis instance (also Oregon/us-west-2, TLS-enabled, free tier). Both share region with the existing [mock-borrow-api](https://github.com/kennethchuaqiyang/mock-borrow-api) deployment.