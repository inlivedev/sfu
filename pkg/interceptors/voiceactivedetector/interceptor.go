package voiceactivedetector

import (
	"context"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const ATTRIBUTE_KEY = "audioLevel"

type InterceptorFactory struct {
	onNew   func(i *Interceptor)
	context context.Context
}

func NewInterceptor(ctx context.Context) *InterceptorFactory {
	return &InterceptorFactory{
		context: ctx,
	}
}

// NewInterceptor constructs a new ReceiverInterceptor
func (g *InterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	i := new(g.context)

	if g.onNew != nil {
		g.onNew(i)
	}

	return i, nil
}

func (g *InterceptorFactory) OnNew(callback func(i *Interceptor)) {
	g.onNew = callback
}

type Config struct {
	HeadMargin time.Duration
	TailMargin time.Duration
	Threshold  uint8
}

func DefaultConfig() Config {
	return Config{
		HeadMargin: 200 * time.Millisecond,
		TailMargin: 300 * time.Millisecond,
		Threshold:  40,
	}
}

type Interceptor struct {
	context context.Context
	mu      sync.Mutex
	vads    map[string]*VoiceDetector
	config  Config
}

func new(ctx context.Context) *Interceptor {
	return &Interceptor{
		context: ctx,
		mu:      sync.Mutex{},
		config:  DefaultConfig(),
		vads:    make(map[string]*VoiceDetector),
	}
}

func (v *Interceptor) SetConfig(config Config) {
	v.config = config
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (v *Interceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if info.MimeType != webrtc.MimeTypeOpus {
		return writer
	}

	vad := v.getVadByID(info.ID)
	if vad != nil {
		vad.updateStreamInfo(info)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if vad == nil {
		v.vads[info.ID] = newVAD(v.context, v, info)

	}

	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		_ = v.processPacket(info.ID, header)
		return writer.Write(header, payload, attributes)
	})
}

// UnbindLocalStream is called when the Stream is removed. It can be used to clean up any data related to that track.
func (v *Interceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	vad := v.getVadByID(info.ID)
	if vad != nil {
		vad.Stop()
	}

	delete(v.vads, info.ID)
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

func (v *Interceptor) getVadByID(id string) *VoiceDetector {
	v.mu.Lock()
	defer v.mu.Unlock()

	vad, ok := v.vads[id]
	if ok {
		return vad
	}

	return nil
}

func (v *Interceptor) processPacket(id string, header *rtp.Header) rtp.AudioLevelExtension {
	audioData := v.getAudioLevel(id, header)
	if audioData.Level == 0 {
		return rtp.AudioLevelExtension{}
	}

	vad := v.getVadByID(id)
	if vad == nil {
		glog.Error("vad: not found vad for track id", id)
		return rtp.AudioLevelExtension{}
	}

	vad.addPacket(header, audioData.Level)

	return audioData
}

func (v *Interceptor) getConfig() Config {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.config
}

func (v *Interceptor) getAudioLevel(id string, header *rtp.Header) rtp.AudioLevelExtension {
	audioLevel := rtp.AudioLevelExtension{}
	headerID := v.getAudioLevelExtensionID(id)
	if headerID != 0 {
		ext := header.GetExtension(headerID)
		_ = audioLevel.Unmarshal(ext)
	}

	return audioLevel
}

func RegisterAudioLevelHeaderExtension(m *webrtc.MediaEngine) {
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}
}

func (v *Interceptor) getAudioLevelExtensionID(id string) uint8 {
	v.mu.Lock()
	defer v.mu.Unlock()

	vad, ok := v.vads[id]
	if ok {
		for _, extension := range vad.streamInfo.RTPHeaderExtensions {
			if extension.URI == sdp.AudioLevelURI {
				return uint8(extension.ID)
			}
		}
	}

	return 0
}

// AddAudioTrack adds audio track to interceptor
func (v *Interceptor) AddAudioTrack(t webrtc.TrackLocal) *VoiceDetector {
	if t.Kind() != webrtc.RTPCodecTypeAudio {
		glog.Error("vad: track is not audio track")
		return nil
	}

	vad := v.getVadByID(t.ID())
	if vad == nil {
		v.mu.Lock()
		vad = newVAD(v.context, v, nil)
		v.vads[t.ID()] = vad
		v.mu.Unlock()
	}

	vad.UpdateTrack(t.ID(), t.StreamID())

	return vad
}
