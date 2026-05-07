package challenger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyFormat(t *testing.T) {
	c := New(nil, "ac", 0)
	assert.Equal(t, "ac:1.2.3.4", c.key("1.2.3.4"))
	assert.Equal(t, "ac:2001:db8::1", c.key("2001:db8::1"))
}

func TestUpdateAddresses_NoChange(t *testing.T) {
	c := New([]string{"localhost:6379"}, "test", 0)
	original := c.clients[0]
	c.UpdateAddresses([]string{"localhost:6379"})
	// Should not recreate clients
	assert.Same(t, original, c.clients[0])
}

func TestUpdateAddresses_Change(t *testing.T) {
	c := New([]string{"localhost:6379"}, "test", 0)
	c.UpdateAddresses([]string{"localhost:6380"})
	assert.Equal(t, []string{"localhost:6380"}, c.addresses)
	c.Close()
}

func TestChallenge_NoBackends(t *testing.T) {
	c := New(nil, "test", 0)
	// Should not panic with no backends
	err := c.Challenge("1.2.3.4", 5*time.Minute, 0)
	assert.NoError(t, err)
}

func TestUnchallenge_NoBackends(t *testing.T) {
	c := New(nil, "test", 0)
	err := c.Unchallenge("1.2.3.4")
	assert.NoError(t, err)
}

func TestIsChallenged_NoBackends(t *testing.T) {
	c := New(nil, "test", 0)
	challenged, err := c.IsChallenged("1.2.3.4")
	require.NoError(t, err)
	assert.False(t, challenged)
}
