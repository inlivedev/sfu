package sfu

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrMetaNotFound = errors.New("meta: metadata not found")
)

type Metadata struct {
	mu                 sync.RWMutex
	m                  map[string]interface{}
	onChangedCallbacks []func(key string, value interface{})
}

func NewMetadata() *Metadata {
	return &Metadata{
		mu:                 sync.RWMutex{},
		m:                  make(map[string]interface{}),
		onChangedCallbacks: make([]func(key string, value interface{}), 0),
	}
}

func (m *Metadata) Set(key string, value interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.m[key] = value
	m.onChanged(key, value)
}

func (m *Metadata) Get(key string) (interface{}, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, ok := m.m[key]; !ok {
		return nil, ErrMetaNotFound
	}
	return m.m[key], nil
}

func (m *Metadata) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.m, key)
	m.onChanged(key, nil)
}

func (m *Metadata) ForEach(f func(key string, value interface{})) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for k, v := range m.m {
		f(k, v)
	}
}

func (m *Metadata) onChanged(key string, value interface{}) {
	for _, f := range m.onChangedCallbacks {
		f(key, value)
	}
}

func (m *Metadata) OnChanged(ctx context.Context, f func(key string, value interface{})) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nextIdx := len(m.onChangedCallbacks)
	m.onChangedCallbacks = append(m.onChangedCallbacks, f)

	go func() {
		localCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		<-localCtx.Done()

		for i, _ := range m.onChangedCallbacks {
			if nextIdx == i {
				m.onChangedCallbacks = append(m.onChangedCallbacks[:i], m.onChangedCallbacks[i+1:]...)
				return
			}
		}
	}()
}
