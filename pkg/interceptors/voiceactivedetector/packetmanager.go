package voiceactivedetector

import (
	"errors"
	"sync"
)

var (
	errPacketReleased       = errors.New("packet has been released")
	errFailedToCastDataPool = errors.New("failed to cast pool")
)

type PacketManager struct {
	RPool *sync.Pool
	Pool  *sync.Pool
}

func newPacketManager() *PacketManager {
	return &PacketManager{
		RPool: &sync.Pool{
			New: func() interface{} {
				return &RetainablePacket{}
			},
		},
		Pool: &sync.Pool{
			New: func() interface{} {
				return &VoicePacketData{}
			},
		},
	}
}

func (m *PacketManager) NewPacket(seqNo uint16, timestamp uint32, audioLevel uint8, isVoice bool) (*RetainablePacket, error) {
	p, ok := m.RPool.Get().(*RetainablePacket)
	if !ok {
		return nil, errFailedToCastDataPool
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.data, ok = m.Pool.Get().(*VoicePacketData)
	if !ok {
		return nil, errFailedToCastDataPool
	}

	p.count = 1
	p.onRelease = m.releasePacket
	p.data.SequenceNo = seqNo
	p.data.Timestamp = timestamp
	p.data.AudioLevel = audioLevel
	p.data.IsVoice = isVoice

	return p, nil
}

func (m *PacketManager) releasePacket(p *RetainablePacket) {
	m.RPool.Put(p)
	m.Pool.Put(p.data)
}

type RetainablePacket struct {
	onRelease func(*RetainablePacket)
	mu        sync.RWMutex
	count     int

	data *VoicePacketData
}

func (p *RetainablePacket) Data() *VoicePacketData {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.data
}

func (p *RetainablePacket) Retain() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.count == 0 {
		// already released
		return errPacketReleased
	}
	p.count++
	return nil
}

func (p *RetainablePacket) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count--

	if p.count == 0 {
		// release back to pool
		p.onRelease(p)
		p.data = nil
	}
}
