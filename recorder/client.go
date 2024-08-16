package recorder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/quic-go/quic-go"
)

type QuicConfig struct {
	Host     string
	Port     int
	CertFile string
	KeyFile  string
}

func NewQuicClient(ctx context.Context, config *QuicConfig) (quic.Connection, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", config.Host, config.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on UDP: %w", err)
	}
	defer udpConn.Close()
	cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate and key: %w", err)
	}

	certPool := x509.NewCertPool()
	certBytes, err := os.ReadFile(config.CertFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate file: %w", err)
	}
	certPool.AppendCertsFromPEM(certBytes)
	tlsConfig := &tls.Config{
		RootCAs:            certPool,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := quic.DialEarly(ctx, udpConn, addr, tlsConfig, &quic.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to dial QUIC connection: %w", err)
	}

	return conn, nil
}
