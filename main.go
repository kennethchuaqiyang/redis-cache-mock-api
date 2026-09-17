package main

// redis-cache-mock-api
//
// Two endpoints, cache-aside pattern backed by Redis, source of truth is
// the same Postgres DB used by mock-borrow-api (mock-borrow-api-db on Neon).
//
//   GET  /api/user?user_id=1
//     - cache miss (first time, or after an update): read from Postgres,
//       write to Redis with TTL, respond with X-Cache: MISS
//     - cache hit (before TTL expiry): serve straight from Redis,
//       respond with X-Cache: HIT
//     - Redis handles TTL expiry natively (active expiration) — an
//       expired key is simply gone, no lazy check needed on our side
//
//   PUT /api/user/update   body: {"user_id": 1, "salary": 5000}
//     - if new salary == current DB value            -> {"message":"No update"}
//     - if DB write fails                             -> {"message":"No Success"}
//     - if DB write succeeds                           -> delete cache key,
//                                                          {"message":"Success"}
//
// Env vars:
//   DATABASE_URL        Postgres connection string (Neon)
//   REDIS_URL           e.g. redis://localhost:6379/0
//   PORT                for Render compatibility (defaults to 8080)
//   CACHE_TTL_SECONDS   defaults to 30
//
// Dependencies (go.mod):
//   github.com/jackc/pgx/v5/pgxpool
//   github.com/redis/go-redis/v9

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var (
	dbPool   *pgxpool.Pool
	rdb      *redis.Client
	cacheTTL time.Duration
)

type user struct {
	UserID   int    `json:"user_id"`
	Username string `json:"username"`
	Location string `json:"location"`
	Salary   int    `json:"salary"`
}

type updateRequest struct {
	UserID int `json:"user_id"`
	Salary int `json:"salary"`
}

type updateResponse struct {
	Message string `json:"message"`
}

func main() {
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	var err error
	dbPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("failed to connect to Postgres: %v", err)
	}
	defer dbPool.Close()

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("invalid REDIS_URL: %v", err)
	}
	rdb = redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("failed to connect to Redis: %v", err)
	}

	cacheTTL = 30 * time.Second
	if v := os.Getenv("CACHE_TTL_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			cacheTTL = time.Duration(secs) * time.Second
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/user", getUserHandler)
	mux.HandleFunc("/api/user/update", updateUserHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("redis-cache-mock-api listening on :%s (ttl=%s)", port, cacheTTL)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func cacheKey(userID int) string {
	return fmt.Sprintf("user:%d", userID)
}

func echoBrowserHeader(w http.ResponseWriter, r *http.Request) {
	browser := r.Header.Get("X-Browser")
	if browser == "" {
		browser = r.Header.Get("User-Agent")
	}
	w.Header().Set("X-Browser", browser)
}

func getUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Query().Get("user_id")
	userID, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "user_id is required and must be an integer", http.StatusBadRequest)
		return
	}

	echoBrowserHeader(w, r)
	w.Header().Set("X-User-Id", idStr)
	w.Header().Set("Content-Type", "application/json")

	ctx := r.Context()
	key := cacheKey(userID)

	// Try cache first.
	cached, err := rdb.Get(ctx, key).Result()
	if err == nil {
		// Cache hit. Report remaining TTL for the cookie.
		ttl, _ := rdb.TTL(ctx, key).Result()
		setCacheCookie(w, key, ttl)
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cached))
		return
	}
	if !errors.Is(err, redis.Nil) {
		http.Error(w, "cache error", http.StatusInternalServerError)
		return
	}

	// Cache miss: read from Postgres.
	u, err := fetchUserFromDB(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	body, err := json.Marshal(u)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}

	if err := rdb.Set(ctx, key, body, cacheTTL).Err(); err != nil {
		log.Printf("warning: failed to populate cache for %s: %v", key, err)
	}

	setCacheCookie(w, key, cacheTTL)
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func setCacheCookie(w http.ResponseWriter, key string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:   "cache_key",
		Value:  key,
		MaxAge: int(ttl.Seconds()),
		Path:   "/",
	})
}

func fetchUserFromDB(ctx context.Context, userID int) (*user, error) {
	row := dbPool.QueryRow(ctx,
		`SELECT user_id, username, location, salary FROM users WHERE user_id = $1`,
		userID,
	)
	var u user
	if err := row.Scan(&u.UserID, &u.Username, &u.Location, &u.Salary); err != nil {
		return nil, err
	}
	return &u, nil
}

func updateUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	echoBrowserHeader(w, r)
	w.Header().Set("X-User-Id", strconv.Itoa(req.UserID))
	w.Header().Set("Content-Type", "application/json")

	ctx := r.Context()

	current, err := fetchUserFromDB(ctx, req.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeUpdateResult(w, http.StatusInternalServerError, "No Success")
		return
	}

	if current.Salary == req.Salary {
		writeUpdateResult(w, http.StatusOK, "No update")
		return
	}

	tag, err := dbPool.Exec(ctx,
		`UPDATE users SET salary = $1 WHERE user_id = $2`,
		req.Salary, req.UserID,
	)
	if err != nil || tag.RowsAffected() == 0 {
		writeUpdateResult(w, http.StatusInternalServerError, "No Success")
		return
	}

	// Invalidate the cache so the next GET is a MISS with fresh data.
	if err := rdb.Del(ctx, cacheKey(req.UserID)).Err(); err != nil {
		log.Printf("warning: failed to invalidate cache for user %d: %v", req.UserID, err)
	}

	writeUpdateResult(w, http.StatusOK, "Success")
}

func writeUpdateResult(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(updateResponse{Message: message})
}
