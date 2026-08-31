package challenger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Challenger writes challenge keys to redis-compatible backends.
type Challenger struct {
	mu        sync.RWMutex
	clients   []*redis.Client
	addresses []string
	keyPrefix string
	timeout   time.Duration
	db        int
}

// New creates a new Challenger with the given backends and key prefix.
func New(addresses []string, keyPrefix string, db int) *Challenger {
	c := &Challenger{
		keyPrefix: keyPrefix,
		timeout:   200 * time.Millisecond,
		db:        db,
	}
	c.updateClients(addresses)
	return c
}

// updateClients recreates clients if addresses changed.
func (c *Challenger) updateClients(addresses []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close old clients
	for _, client := range c.clients {
		_ = client.Close()
	}

	c.addresses = addresses
	c.clients = make([]*redis.Client, len(addresses))
	for i, addr := range addresses {
		c.clients[i] = redis.NewClient(&redis.Options{
			Addr:         addr,
			DB:           c.db,
			ReadTimeout:  c.timeout,
			WriteTimeout: c.timeout,
			DialTimeout:  c.timeout,
			PoolSize:     4,
		})
	}
}

// UpdateAddresses updates the backend addresses (for config hot-reload).
func (c *Challenger) UpdateAddresses(addresses []string) {
	c.mu.RLock()
	same := len(addresses) == len(c.addresses)
	if same {
		for i, a := range addresses {
			if a != c.addresses[i] {
				same = false
				break
			}
		}
	}
	c.mu.RUnlock()
	if same {
		return
	}
	c.updateClients(addresses)
}

// challengeScript sets a challenge key with escalate-only semantics:
// - Difficulty only increases (higher wins)
// - TTL only extends (longer wins)
var challengeScript = redis.NewScript(`
local t = redis.call('TTL', KEYS[1])
local cur = tonumber(redis.call('GET', KEYS[1]) or "0") or 0
local new_ttl = tonumber(ARGV[1])
local new_d = tonumber(ARGV[2]) or 0
if new_d > cur or new_ttl > t then
    redis.call('SET', KEYS[1], tostring(math.max(cur, new_d)), 'EX', math.max(t, new_ttl))
end
return t
`)

// Challenge sets a challenge key for the given IP with a difficulty level.
// Escalate-only: difficulty only increases, TTL only extends.
// Writes to all backends (fan-out). Fails silently on individual backend errors.
func (c *Challenger) Challenge(ip string, duration time.Duration, difficulty int) error {
	key := c.key(ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	seconds := int64(duration.Seconds())
	var lastErr error
	for _, client := range clients {
		if err := challengeScript.Run(ctx, client, []string{key}, seconds, difficulty).Err(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Unchallenge removes the challenge key for the given IP.
// Deletes from all backends (fan-out).
func (c *Challenger) Unchallenge(ip string) error {
	key := c.key(ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	var lastErr error
	for _, client := range clients {
		if err := client.Del(ctx, key).Err(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// GetChallengeDifficulty returns the challenge difficulty for an IP.
// Returns -1 if not challenged, 0 if challenged with no specific difficulty,
// or a positive int for a specific difficulty level.
func (c *Challenger) GetChallengeDifficulty(ip string) (int, error) {
	key := c.key(ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	if len(clients) == 0 {
		return -1, nil
	}

	res, err := clients[0].Get(ctx, key).Result()
	if err == redis.Nil {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	d := 0
	_, _ = fmt.Sscanf(res, "%d", &d)
	return d, nil
}

// IsChallenged checks if an IP is challenged (key exists, regardless of value).
func (c *Challenger) IsChallenged(ip string) (bool, error) {
	d, err := c.GetChallengeDifficulty(ip)
	return d >= 0, err
}

// Close closes all clients.
func (c *Challenger) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range c.clients {
		_ = client.Close()
	}
	c.clients = nil
}

func (c *Challenger) key(ip string) string {
	return fmt.Sprintf("%s:%s", c.keyPrefix, ip)
}

// DB returns the configured database number.
func (c *Challenger) DB() int { return c.db }

// KeyPrefix returns the configured key prefix.
func (c *Challenger) KeyPrefix() string { return c.keyPrefix }
