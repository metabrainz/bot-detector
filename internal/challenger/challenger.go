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

// challengeScript sets a key only if it doesn't exist or new TTL is longer than remaining.
var challengeScript = redis.NewScript(`
local t = redis.call('TTL', KEYS[1])
if t < tonumber(ARGV[1]) then
    redis.call('SET', KEYS[1], '', 'EX', ARGV[1])
end
return t
`)

// Challenge sets a challenge key for the given IP.
// Only extends TTL, never shortens — a shorter chain can't shrink a longer challenge.
// Writes to all backends (fan-out). Fails silently on individual backend errors.
func (c *Challenger) Challenge(ip string, duration time.Duration) error {
	key := c.key(ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	seconds := int64(duration.Seconds())
	var lastErr error
	for _, client := range clients {
		if err := challengeScript.Run(ctx, client, []string{key}, seconds).Err(); err != nil {
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

// IsChallenged checks if an IP is challenged.
func (c *Challenger) IsChallenged(ip string) (bool, error) {
	key := c.key(ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	if len(clients) == 0 {
		return false, nil
	}

	n, err := clients[0].Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
