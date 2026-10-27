package pacer

import (
	"github.com/pion/interceptor"
)

// StreamInfoInterceptorFactory builds interceptors that pass each local
// stream's info to a LeakyBucketPacer through SetStreamInfo. The gcc
// interceptor only gives the pacer the SSRC of a stream, so without this the
// pacer can't tell audio from video or route RTX packets.
type StreamInfoInterceptorFactory struct {
	pacer func() *LeakyBucketPacer
}

// NewStreamInfoInterceptorFactory returns a factory whose interceptors report
// local streams to the pacer that pacer returns. pacer is called for every
// bound stream, so it can return a pacer that is created after the factory;
// streams bound while it returns nil are ignored.
func NewStreamInfoInterceptorFactory(pacer func() *LeakyBucketPacer) *StreamInfoInterceptorFactory {
	return &StreamInfoInterceptorFactory{pacer: pacer}
}

// NewInterceptor implements interceptor.Factory.
func (f *StreamInfoInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &streamInfoInterceptor{pacer: f.pacer}, nil
}

type streamInfoInterceptor struct {
	interceptor.NoOp
	pacer func() *LeakyBucketPacer
}

// BindLocalStream implements interceptor.Interceptor.
func (i *streamInfoInterceptor) BindLocalStream(
	info *interceptor.StreamInfo, writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	if p := i.pacer(); p != nil {
		p.SetStreamInfo(info)
	}

	return writer
}
