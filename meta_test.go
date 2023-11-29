package sfu

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetadata(t *testing.T) {
	// Test NewMetadata function
	m := NewMetadata()
	if m == nil {
		t.Error("NewMetadata returned nil")
	}

	_, err := m.Get("key1")
	require.Equal(t, ErrMetaNotFound, err)

	// Test Set and Get methods
	m.Set("key1", "value1")
	m.Set("key2", 123)
	if value, _ := m.Get("key1"); value != "value1" {
		t.Errorf("Get returned %v, expected %v", value, "value1")
	}
	if value, _ := m.Get("key2"); value != 123 {
		t.Errorf("Get returned %v, expected %v", value, 123)
	}

	// Test Delete method
	m.Delete("key1")
	if value, _ := m.Get("key1"); value != nil {
		t.Errorf("Get returned %v, expected nil", value)
	}

	// Test ForEach method
	m.Set("key3", "value3")
	m.Set("key4", 456)
	m.ForEach(func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
	})

	// Test OnChanged method
	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan struct{})
	m.OnChanged(ctx, func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
		ch <- struct{}{}
	})
	go func() {
		m.Set("key5", "value5")
	}()

	<-ch

	// cancel the listener above
	cancel()

	// Test OnChanged method with cancel
	var state = true
	ctx1, cancel1 := context.WithCancel(context.Background())
	m.OnChanged(ctx1, func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
		state = false
	})

	cancel1()

	m.Set("key6", "value6")

	require.Equal(t, true, state)

}
