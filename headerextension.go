package sfu

import (
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const (
	maxOneByteExtensionID      = 14
	maxOneByteExtensionPayload = 16
)

// headerExtensionRewriter translates the RTP header extensions of a forwarded packet from the
// publisher's negotiated IDs to the subscriber's. TrackLocalStaticRTP only rewrites SSRC and payload
// type, so without it a subscriber receives the publisher's extension IDs and values. Browsers demux
// BUNDLEd RTP by the MID extension first, so a forwarded publisher MID routes the stream to whichever
// of the subscriber's m-sections has the same mid.
//
// Extensions are mapped by URI. The subscriber's own mid replaces the MID value, RID and repaired RID
// are dropped because a subscriber receives a single layer, and extensions the subscriber did not
// negotiate are dropped.
//
// rewrite must be serialized by the caller, it reuses one extension slice for every packet.
type headerExtensionRewriter struct {
	client *Client
	// publisher's negotiated header extensions, nil when unknown (relay tracks)
	publisher   []webrtc.RTPHeaderExtensionParameter
	transceiver atomic.Pointer[webrtc.RTPTransceiver]

	// the transceiver and negotiation the table below was resolved for
	resolvedTransceiver *webrtc.RTPTransceiver
	resolvedNegotiation uint64
	resolved            bool

	// publisher extension IDs and the subscriber IDs they are forwarded as
	mappings   []extensionMapping
	midID      uint8
	mid        []byte
	pending    []extensionMapping
	payloads   [][]byte
	extensions []rtp.Extension
}

type extensionMapping struct {
	from, to uint8
}

func newHeaderExtensionRewriter(c *Client, publisher []webrtc.RTPHeaderExtensionParameter) *headerExtensionRewriter {
	return &headerExtensionRewriter{
		client:     c,
		publisher:  publisher,
		mappings:   make([]extensionMapping, 0, 8),
		pending:    make([]extensionMapping, 0, 8),
		payloads:   make([][]byte, 0, 8),
		extensions: make([]rtp.Extension, 0, 8),
	}
}

// setTransceiver sets the subscriber transceiver the track is sent on.
func (r *headerExtensionRewriter) setTransceiver(t *webrtc.RTPTransceiver) {
	r.transceiver.Store(t)
}

// rewrite replaces the extensions of h with the ones the subscriber should receive.
// h must be a copy owned by this subscriber: its Extensions slice is replaced, never modified,
// because forwarded copies of a packet share the publisher's Extensions backing array.
func (r *headerExtensionRewriter) rewrite(h *rtp.Header) {
	r.resolveIfChanged()

	r.pending = r.pending[:0]
	r.payloads = r.payloads[:0]
	profile := uint16(rtp.ExtensionProfileOneByte)

	for _, m := range r.mappings {
		payload := h.GetExtension(m.from)
		if payload == nil {
			continue
		}

		r.pending = append(r.pending, m)
		r.payloads = append(r.payloads, payload)

		if !fitsOneByteExtension(m.to, payload) {
			profile = rtp.ExtensionProfileTwoByte
		}
	}

	if r.midID != 0 {
		r.pending = append(r.pending, extensionMapping{to: r.midID})
		r.payloads = append(r.payloads, r.mid)

		if !fitsOneByteExtension(r.midID, r.mid) {
			profile = rtp.ExtensionProfileTwoByte
		}
	}

	h.Extensions = r.extensions[:0]
	h.Extension = len(r.pending) > 0
	h.ExtensionProfile = 0

	if !h.Extension {
		return
	}

	h.ExtensionProfile = profile

	for i, m := range r.pending {
		// the IDs and payloads were checked against the profile above
		_ = h.SetExtension(m.to, r.payloads[i])
	}

	// keep the backing array if append had to grow it
	r.extensions = h.Extensions[:0]
}

func fitsOneByteExtension(id uint8, payload []byte) bool {
	return id <= maxOneByteExtensionID && len(payload) > 0 && len(payload) <= maxOneByteExtensionPayload
}

// resolveIfChanged rebuilds the ID table when the transceiver is set or a negotiation completed,
// since the subscriber's mid and extension IDs are only known after negotiation.
func (r *headerExtensionRewriter) resolveIfChanged() {
	transceiver := r.transceiver.Load()
	negotiation := r.client.negotiations.Load()

	if r.resolved && transceiver == r.resolvedTransceiver && negotiation == r.resolvedNegotiation {
		return
	}

	r.resolved = true
	r.resolvedTransceiver = transceiver
	r.resolvedNegotiation = negotiation

	if transceiver == nil || transceiver.Sender() == nil {
		r.build(nil, "")

		return
	}

	r.build(transceiver.Sender().GetParameters().HeaderExtensions, transceiver.Mid())
}

// build maps the publisher's extensions to the subscriber's negotiated IDs by URI.
func (r *headerExtensionRewriter) build(subscriber []webrtc.RTPHeaderExtensionParameter, mid string) {
	r.mappings = r.mappings[:0]
	r.midID = 0
	r.mid = r.mid[:0]

	if mid != "" {
		if id := extensionID(subscriber, sdp.SDESMidURI); id != 0 {
			r.midID = id
			r.mid = append(r.mid, mid...)
		}
	}

	for _, e := range r.publisher {
		switch e.URI {
		case sdp.SDESMidURI, sdp.SDESRTPStreamIDURI, sdp.SDESRepairRTPStreamIDURI:
			continue
		}

		if e.ID <= 0 || e.ID > 255 {
			continue
		}

		if id := extensionID(subscriber, e.URI); id != 0 {
			r.mappings = append(r.mappings, extensionMapping{from: uint8(e.ID), to: id})
		}
	}
}

// extensionID returns the ID negotiated for uri, or 0 when it is not negotiated.
func extensionID(extensions []webrtc.RTPHeaderExtensionParameter, uri string) uint8 {
	for _, e := range extensions {
		if e.URI == uri && e.ID > 0 && e.ID < 256 {
			return uint8(e.ID)
		}
	}

	return 0
}
