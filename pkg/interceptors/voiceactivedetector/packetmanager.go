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
	Pool *sync.Pool
}

func newPacketManager() *PacketManager {
	return &PacketManager{
		Pool: &sync.Pool{
			New: func() interface{} {
				return &VoicePacketData{}
			},
		},
	}
}

func (m *PacketManager) NewPacket(seqNo uint16, timestamp uint32, audioLevel uint8) (*RetainablePacket, error) {

	p := &RetainablePacket{
		onRelease: m.releasePacket,
		// new packets have retain count of 1
		count: 1,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	var ok bool
	p.data, ok = m.Pool.Get().(*VoicePacketData)
	if !ok {
		return nil, errFailedToCastDataPool
	}

	p.data.SequenceNo = seqNo
	p.data.Timestamp = timestamp
	p.data.AudioLevel = audioLevel

	return p, nil
}

func (m *PacketManager) releasePacket(data *VoicePacketData) {
	m.Pool.Put(data)
}

type RetainablePacket struct {
	onRelease func(*VoicePacketData)
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
		p.onRelease(p.data)
		p.data = nil
	}
}
