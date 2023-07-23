package sfu

import (
	"context"

	"github.com/pion/ice/v2"
)

type UDPMux struct {
	Port    int
	mux     *ice.MultiUDPMuxDefault
	context context.Context
	cancel  context.CancelFunc
}

func NewUDPMux(ctx context.Context, port int) *UDPMux {
	localCtx, cancel := context.WithCancel(ctx)

	mux, err := ice.NewMultiUDPMuxFromPort(port)
	if err != nil {
		panic(err)
	}

	go func() {
		<-localCtx.Done()
		cancel()
		mux.Close()
	}()

	return &UDPMux{
		Port:    port,
		mux:     mux,
		context: localCtx,
		cancel:  cancel,
	}
}

func (u *UDPMux) Close() error {
	u.cancel()
	return u.mux.Close()
}
