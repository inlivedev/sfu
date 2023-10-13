package sfu

import (
	"context"
	"sync/atomic"

	"github.com/golang/glog"
	"github.com/pion/rtp"
)

type sendPacket struct {
	packet  *rtp.Packet
	quality QualityLevel
}
type switchController struct {
	qualityChanged atomic.Bool
	QualityLevel   QualityLevel
	track          *simulcastClientTrack
	channel        chan sendPacket
	packets        []sendPacket
}

func newSwitchController(t *simulcastClientTrack) *switchController {
	s := &switchController{
		track:   t,
		channel: make(chan sendPacket, 100),
		packets: make([]sendPacket, 0),
	}

	s.startSender()
	s.startReceiver()

	return s
}

// this is not guaranteed to be in order
func (s *switchController) sendPacket(packet *rtp.Packet, quality QualityLevel) {
	s.channel <- sendPacket{
		packet:  packet,
		quality: quality,
	}
}

func (s *switchController) startReceiver() {
	go func() {
		for {
			ctx, cancel := context.WithCancel(s.track.GetRemoteTrack().context)
			defer cancel()

			select {
			case <-ctx.Done():
				return
			case p := <-s.channel:
				if len(s.packets) != 0 {
					prevPacket := s.packets[len(s.packets)-1]
					if prevPacket.quality != p.quality {
						s.qualityChanged.Store(true)
						glog.Infof("switching to quality %s", p.quality)
					}
				}
				s.packets = append(s.packets, p)
			}
		}
	}()
}

// TODO:
// use this to make sure the packet sent in order
func (s *switchController) switchQuality(quality QualityLevel) {
	s.qualityChanged.Store(true)
	s.QualityLevel = quality
}

func (s *switchController) startSender() {
	go func() {
		var prevTimestamp uint32
		var prevQuality QualityLevel
		for {
			//TODO: panic nil pointer here, s.track is nil
			ctx, cancel := context.WithCancel(s.track.GetRemoteTrack().context)
			defer cancel()

			select {
			case <-ctx.Done():
				return
			default:
				if len(s.packets) > 0 {
					p := s.packets[0]
					s.packets = s.packets[1:]

					// this to make sure all packets from the same frame are sent
					// before we switch track
					if p.packet.Timestamp > prevTimestamp {
						// all frame packets are sent
						// we can switch quality here
						if s.qualityChanged.Load() && prevQuality == p.quality {
							// skip if packet still from previous quality
							continue
						}

					}

					prevTimestamp = p.packet.Timestamp
					prevQuality = p.quality

					s.writeRtp(p.packet, p.quality)
				}
			}
		}
	}()
}

func (s *switchController) writeRtp(p *rtp.Packet, quality QualityLevel) {
	s.track.mu.Lock()
	defer s.track.mu.Unlock()

	t := s.track

	// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
	sequenceDelta := uint16(0)
	// credit to https://github.com/k0nserv for helping me with this on Pion Slack channel
	switch quality {
	case QualityHigh:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackHighBaseTS) - t.remoteTrack.remoteTrackHighBaseTS)
		sequenceDelta = t.remoteTrack.highSequence - t.remoteTrack.lastHighSequence
	case QualityMid:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackMidBaseTS) - t.remoteTrack.remoteTrackMidBaseTS)
		sequenceDelta = t.remoteTrack.midSequence - t.remoteTrack.lastMidSequence
	case QualityLow:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackLowBaseTS) - t.remoteTrack.remoteTrackLowBaseTS)
		sequenceDelta = t.remoteTrack.lowSequence - t.remoteTrack.lastLowSequence
	}

	t.sequenceNumber.Add(uint32(sequenceDelta))
	p.SequenceNumber = uint16(t.sequenceNumber.Load())

	if err := s.track.localTrack.WriteRTP(p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
}
