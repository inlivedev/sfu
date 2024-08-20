package recorder

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

type QuicConfig struct {
	Host     string
	Port     int
	CertFile string
	KeyFile  string
}

func GetRandomQuicClient(config []*QuicConfig) (quic.Connection, error) {
	return NewQuicClient(context.Background(), config[rand.Intn(len(config))])
}

func readCertAndKey() *tls.Config {
	cert, err := tls.LoadX509KeyPair("server.cert", "server.key")
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"quic-echo-example"},
		InsecureSkipVerify: true,
	}
}

func NewQuicClient(ctx context.Context, config *QuicConfig) (quic.Connection, error) {
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on UDP: %w", err)
	}
	defer udpConn.Close()
	tlsConfig := readCertAndKey()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, "127.0.0.1:9000", tlsConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to dial QUIC connection: %w", err)
	}

	return conn, nil
}
