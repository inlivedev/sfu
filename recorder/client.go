package recorder

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/quic-go/quic-go"
)

type QuicConfig struct {
	Host     string
	Port     int
	CertFile string
	KeyFile  string
}

type ClientConfig struct {
	ClientId string
	FileName string
}

func GetRandomQuicClient(clientConfig ClientConfig, config []*QuicConfig) (quic.Connection, error) {
	return NewQuicClient(context.Background(), clientConfig, config[rand.Intn(len(config))])
}

func readCertAndKey(config *QuicConfig) *tls.Config {
	cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"samespace-recorder"},
		InsecureSkipVerify: true,
	}
}

func NewQuicClient(ctx context.Context, clientConfig ClientConfig, config *QuicConfig) (quic.Connection, error) {
	var conn quic.Connection
	var err error

	tlsConfig := readCertAndKey(config)
	address := fmt.Sprintf("%s:%d", config.Host, config.Port)

	for retries := 0; retries < 5; retries++ {
		fmt.Printf("Attempting connection (attempt %d)...\n", retries+1)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		conn, err = quic.DialAddr(ctx, address, tlsConfig, &quic.Config{
			EnableDatagrams: true,
		})
		if err == nil {
			j, _ := json.Marshal(clientConfig)
			err := conn.SendDatagram(j)
			if err != nil {
				return nil, err
			}
			return conn, nil
		}

		fmt.Printf("Connection failed (attempt %d): %v\n", retries+1, err)
		time.Sleep(time.Duration(2<<retries) * time.Second) // backoff exponentially
	}

	return nil, fmt.Errorf("failed to establish QUIC connection after retries: %w", err)
}
