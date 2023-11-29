package sfu

import (
	"context"
	"testing"
)

func TestMetadata(t *testing.T) {
	// Test NewMetadata function
	m := NewMetadata()
	if m == nil {
		t.Error("NewMetadata returned nil")
	}

	// Test Set and Get methods
	m.Set("key1", "value1")
	m.Set("key2", 123)
	if value := m.Get("key1"); value != "value1" {
		t.Errorf("Get returned %v, expected %v", value, "value1")
	}
	if value := m.Get("key2"); value != 123 {
		t.Errorf("Get returned %v, expected %v", value, 123)
	}

	// Test Delete method
	m.Delete("key1")
	if value := m.Get("key1"); value != nil {
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
	defer cancel()
	ch := make(chan struct{})
	m.OnChanged(ctx, func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
		ch <- struct{}{}
	})
	go func() {
		m.Set("key5", "value5")
	}()

	<-ch
}
