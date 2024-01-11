package playoutdelay

import (
	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type InterceptorFactory struct {
	minDelay, maxDelay uint16
}

func NewInterceptor(minDelay, maxDelay uint16) *InterceptorFactory {
	return &InterceptorFactory{
		minDelay: minDelay,
		maxDelay: maxDelay,
	}
}

// NewInterceptor constructs a new ReceiverInterceptor
func (g *InterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	i := new(g.minDelay, g.maxDelay)

	return i, nil
}

type Interceptor struct {
	minDelay uint16
	maxDelay uint16
}

func new(min, max uint16) *Interceptor {
	return &Interceptor{
		minDelay: min,
		maxDelay: max,
	}
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (v *Interceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	extID := v.getHeaderExtensionID(info, PlayoutDelayURI)

	playOutDelay := PlayoutDelayFromValue(v.minDelay, v.maxDelay)

	payloadDelay, err := playOutDelay.Marshal()
	if err != nil {
		glog.Error("error on marshal playout delay payload", err)
		return writer
	}

	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		newHeader := v.addPlayoutDelay(info, header, extID, payloadDelay)
		return writer.Write(newHeader, payload, attributes)
	})
}

// UnbindLocalStream is called when the Stream is removed. It can be used to clean up any data related to that track.
func (v *Interceptor) UnbindLocalStream(info *interceptor.StreamInfo) {

}

// BindRemoteStream lets you modify any incoming RTP packets. It is called once for per RemoteStream. The returned method
// will be called once per rtp packet.
func (v *Interceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	return reader
}

func (v *Interceptor) UnbindRemoteStream(info *interceptor.StreamInfo) {

}

func (v *Interceptor) Close() error {

	return nil
}

// BindRTCPReader lets you modify any incoming RTCP packets. It is called once per sender/receiver, however this might
// change in the future. The returned method will be called once per packet batch.
func (v *Interceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return reader
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (v *Interceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return writer
}

func (v *Interceptor) addPlayoutDelay(info *interceptor.StreamInfo, header *rtp.Header, extID uint8, payload []byte) *rtp.Header {
	if extID == 0 {
		return header
	}

	if payload == nil {
		return header
	}

	err := header.SetExtension(extID, payload)
	if err != nil {
		glog.Error("error on set playout delay extension: ", err)
		return header
	}

	return header
}

func RegisterPlayoutDelayHeaderExtension(m *webrtc.MediaEngine) {
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: PlayoutDelayURI}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: PlayoutDelayURI}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}
}

func (v *Interceptor) getHeaderExtensionID(streamInfo *interceptor.StreamInfo, id string) uint8 {
	for _, extension := range streamInfo.RTPHeaderExtensions {
		if extension.URI == PlayoutDelayURI {
			return uint8(extension.ID)
		}
	}

	return 0
}
