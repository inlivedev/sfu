package sfu

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMetadata(t *testing.T) {
	type dataValue struct {
		key   string
		value interface{}
	}

	// Test NewMetadata function
	m := NewMetadata()
	if m == nil {
		t.Error("NewMetadata returned nil")
	}

	_, err := m.Get("key1")
	require.Equal(t, ErrMetaNotFound, err)

	reqData := map[string]interface{}{
		"key1":  "data1",
		"key2":  "data2",
		"key3":  "data3",
		"key4":  "data4",
		"key5":  "data5",
		"key6":  "data6",
		"key7":  "data7",
		"key8":  "data8",
		"key9":  "data9",
		"key10": "data10",
	}

	// Test OnChanged method

	receivedMetas := make(map[string]interface{})
	chanMeta := make(chan dataValue, len(reqData))
	callback1 := m.OnChanged(func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
		chanMeta <- dataValue{key: key, value: value}

	})

	// Test Set and Get methods
	for k, v := range reqData {
		m.Set(k, v)
		if value, _ := m.Get(k); value != v {
			t.Errorf("Get returned %v, expected %v", value, "value1")
		}
	}

	for i := 0; i < len(reqData); i++ {
		select {
		case data := <-chanMeta:
			receivedMetas[data.key] = data.value
		case <-time.After(100 * time.Millisecond):
			t.Error("timeout waiting for meta")
		}
	}

	require.Equal(t, len(reqData), len(receivedMetas))

	// Test ForEach method
	m.ForEach(func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
	})

	// Test Delete method
	for k := range reqData {
		_ = m.Delete(k)
		if _, err := m.Get(k); err != ErrMetaNotFound {
			t.Errorf("Get returned %v, expected %v", err, ErrMetaNotFound)
		}
	}

	// cancel the listener above
	callback1.Remove()
	close(chanMeta)

	// Test OnChanged method with cancel
	var state = true

	callback2 := m.OnChanged(func(key string, value interface{}) {
		t.Logf("Key: %s, Value: %v", key, value)
		state = false
	})

	callback2.Remove()

	m.Set("key6", "value6")

	require.Equal(t, true, state)
}
