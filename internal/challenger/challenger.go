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
}

// New creates a new Challenger with the given backends and key prefix.
func New(addresses []string, keyPrefix string) *Challenger {
	c := &Challenger{
		keyPrefix: keyPrefix,
		timeout:   200 * time.Millisecond,
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

// Challenge sets a challenge key for the given IP and website with a TTL.
// Writes to all backends (fan-out). Fails silently on individual backend errors.
func (c *Challenger) Challenge(ip, website string, duration time.Duration, reason string) error {
	key := c.key(website, ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	var lastErr error
	for _, client := range clients {
		if err := client.Set(ctx, key, reason, duration).Err(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Unchallenge removes the challenge key for the given IP and website.
// Deletes from all backends (fan-out).
func (c *Challenger) Unchallenge(ip, website string) error {
	key := c.key(website, ip)
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

// IsChalllenged checks if an IP is challenged on a website.
func (c *Challenger) IsChallenged(ip, website string) (bool, string, error) {
	key := c.key(website, ip)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	c.mu.RLock()
	clients := c.clients
	c.mu.RUnlock()

	if len(clients) == 0 {
		return false, "", nil
	}

	val, err := clients[0].Get(ctx, key).Result()
	if err == redis.Nil {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, val, nil
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

func (c *Challenger) key(website, ip string) string {
	return fmt.Sprintf("%s:%s:%s", c.keyPrefix, website, ip)
}
