package sfu

import (
	"context"

	"github.com/pion/ice/v4"
)

type UDPMux struct {
	Port    int
	mux     *ice.MultiUDPMuxDefault
	context context.Context
	cancel  context.CancelFunc
}

func NewUDPMux(ctx context.Context, port int) *UDPMux {
	localCtx, cancel := context.WithCancel(ctx)

	opts := []ice.UDPMuxFromPortOption{
		ice.UDPMuxFromPortWithReadBufferSize(25_000_000),
		ice.UDPMuxFromPortWithWriteBufferSize(25_000_000),
		ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4),
		ice.UDPMuxFromPortWithLoopback(),
	}

	mux, err := ice.NewMultiUDPMuxFromPort(port, opts...)
	if err != nil {
		panic(err)
	}

	go func() {
		defer mux.Close()
		<-localCtx.Done()
		cancel()

	}()

	return &UDPMux{
		Port:    port,
		mux:     mux,
		context: localCtx,
		cancel:  cancel,
	}
}

func (u *UDPMux) Mux() *ice.MultiUDPMuxDefault {
	return u.mux
}

func (u *UDPMux) Close() error {
	u.cancel()
	return u.mux.Close()
}
