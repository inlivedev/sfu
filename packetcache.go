package sfu

import (
	"container/list"
	"errors"
	"sync"

	"github.com/golang/glog"
)

type packetCaches struct {
	mu     sync.RWMutex
	caches *list.List
	init   bool
	Size   uint16
}

type Cache struct {
	SeqNum    uint16
	Timestamp uint32
	BaseSeq   uint16
	// packet TID
	PTID uint8
	// packet SID
	PSID uint8
	// stream TID
	TID uint8
	// stream SID
	SID uint8
}
type OperationType uint8

const (
	KEEPLAYER      = OperationType(0)
	SCALEDOWNLAYER = OperationType(1)
	SCALEUPLAYER   = OperationType(2)
)

var (
	ErrCantDecide = errors.New("can't decide if upscale or downscale the layer")
	ErrDuplicate  = errors.New("packet sequence already exists in the cache")
)

func newPacketCaches() *packetCaches {
	return &packetCaches{
		mu:     sync.RWMutex{},
		caches: &list.List{},
		init:   false,
		Size:   1000,
	}
}

func (p *packetCaches) Add(seqNum, baseSequence uint16, ts uint32, psid, tsid, sid, tid uint8) {
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()

		if uint16(p.caches.Len()) > p.Size {
			p.caches.Remove(p.caches.Front())
		}
	}()

	newCache := Cache{
		SeqNum:    seqNum,
		BaseSeq:   baseSequence,
		Timestamp: ts,
		PSID:      psid,
		PTID:      tsid,
		TID:       tid,
		SID:       sid,
	}

	if p.caches.Len() == 0 {
		p.caches.PushBack(newCache)

		return
	}

	// add packet in order
Loop:
	for e := p.caches.Back(); e != nil; e = e.Prev() {
		currentCache := e.Value.(Cache)
		if currentCache.SeqNum == seqNum {
			glog.Warning("packet cache: packet sequence ", seqNum, " already exists in the cache, will not adding the packet")

			return
		}

		if currentCache.SeqNum < seqNum && seqNum-currentCache.SeqNum < uint16SizeHalf {
			p.caches.InsertAfter(newCache, e)

			break Loop
		} else if currentCache.SeqNum-seqNum > uint16SizeHalf {
			p.caches.InsertAfter(newCache, e)

			break Loop
		} else if e.Prev() == nil {
			p.caches.PushFront(newCache)

			break Loop
		}
	}
}

// This will provide decided sid and tid that can be used for the current packet
// it can return the same sid and tid if the sequence number is in sequence
// it will decide to upscale or downscale the sid and tid based on the sequence number
func (p *packetCaches) GetDecision(currentSeqNum, currentBaseSeq uint16, currentSID, currentTID uint8) (baseSeq uint16, sid uint8, tid uint8, err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for e := p.caches.Back(); e != nil; e = e.Prev() {
		currentCache := e.Value.(Cache)
		if currentCache.SeqNum == currentSeqNum {
			return currentBaseSeq, currentSID, currentTID, ErrDuplicate
		}

		// the current packet is not late
		if currentCache.SeqNum < currentSeqNum &&
			currentCache.SeqNum-currentSeqNum > uint16SizeHalf && currentSeqNum-currentCache.SeqNum == 1 {
			// next packet is the next sequence number, allowed to upscale or downscale
			return currentCache.BaseSeq, currentSID, currentTID, nil
		}

		if currentCache.SeqNum < currentSeqNum &&
			currentCache.SeqNum-currentSeqNum > uint16SizeHalf && currentSeqNum-currentCache.SeqNum > 1 {
			// next packet is has a gap, can't decide keep the current SID and TID
			return currentCache.BaseSeq, currentCache.SID, currentCache.TID, ErrCantDecide
		}
	}

	// can't decide could be because the cache is empty
	if p.caches.Len() == 0 {
		return currentBaseSeq, currentSID, currentTID, ErrCantDecide
	}

	return currentBaseSeq, currentSID, currentTID, ErrCantDecide
}

func (p *packetCaches) IsAllowToUpscaleDownscale(seqNum uint16) bool {
	if p.caches.Back() == nil {
		return true
	}

	cache, ok := p.caches.Back().Value.(Cache)
	if !ok {
		return false
	}

	if cache.SeqNum < seqNum {
		return true
	}

	return false
}
