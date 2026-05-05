package challenger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyFormat(t *testing.T) {
	c := New(nil, "antibot:challenge")
	assert.Equal(t, "antibot:challenge:mb_prod:1.2.3.4", c.key("mb_prod", "1.2.3.4"))
	assert.Equal(t, "antibot:challenge:mb_test:2001:db8::1", c.key("mb_test", "2001:db8::1"))
}

func TestUpdateAddresses_NoChange(t *testing.T) {
	c := New([]string{"localhost:6379"}, "test")
	original := c.clients[0]
	c.UpdateAddresses([]string{"localhost:6379"})
	// Should not recreate clients
	assert.Same(t, original, c.clients[0])
}

func TestUpdateAddresses_Change(t *testing.T) {
	c := New([]string{"localhost:6379"}, "test")
	c.UpdateAddresses([]string{"localhost:6380"})
	assert.Equal(t, []string{"localhost:6380"}, c.addresses)
	c.Close()
}

func TestChallenge_NoBackends(t *testing.T) {
	c := New(nil, "test")
	// Should not panic with no backends
	err := c.Challenge("1.2.3.4", "mb_prod", 5*time.Minute, "test-reason")
	assert.NoError(t, err)
}

func TestUnchallenge_NoBackends(t *testing.T) {
	c := New(nil, "test")
	err := c.Unchallenge("1.2.3.4", "mb_prod")
	assert.NoError(t, err)
}

func TestIsChallenged_NoBackends(t *testing.T) {
	c := New(nil, "test")
	challenged, reason, err := c.IsChallenged("1.2.3.4", "mb_prod")
	require.NoError(t, err)
	assert.False(t, challenged)
	assert.Empty(t, reason)
}

func TestDuration_UnmarshalYAML(t *testing.T) {
	var d Duration
	err := d.UnmarshalYAML(func(v interface{}) error {
		*(v.(*string)) = "24h"
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, time.Duration(d))
}
