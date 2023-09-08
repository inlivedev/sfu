package sfu

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"testing"

	"github.com/pion/turn/v2"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("PIONS_LOG_DEBUG", "all")
	flag.Set("PIONS_LOG_INFO", "all")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	turnServer := startTurnServer(ctx, "127.0.0.1")
	defer turnServer.Close()

	result := m.Run()

	os.Exit(result)
}

func startTurnServer(ctx context.Context, publicIP string) *turn.Server {
	port := 3478
	users := "user=pass"
	realm := "test"
	threadNum := 1
	flag.Parse()

	if len(publicIP) == 0 {
		log.Fatalf("'public-ip' is required")
	}

	addr, err := net.ResolveUDPAddr("udp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		log.Fatalf("Failed to parse server address: %s", err)
	}

	// Cache -users flag for easy lookup later
	// If passwords are stored they should be saved to your DB hashed using turn.GenerateAuthKey
	usersMap := map[string][]byte{}
	for _, kv := range regexp.MustCompile(`(\w+)=(\w+)`).FindAllStringSubmatch(users, -1) {
		usersMap[kv[1]] = turn.GenerateAuthKey(kv[1], realm, kv[2])
	}

	// Create `numThreads` UDP listeners to pass into pion/turn
	// pion/turn itself doesn't allocate any UDP sockets, but lets the user pass them in
	// this allows us to add logging, storage or modify inbound/outbound traffic
	// UDP listeners share the same local address:port with setting SO_REUSEPORT and the kernel
	// will load-balance received packets per the IP 5-tuple
	listenerConfig := &net.ListenConfig{
		Control: func(network, address string, conn syscall.RawConn) error {
			var operr error
			if err = conn.Control(func(fd uintptr) {
				operr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}

			return operr
		},
	}

	relayAddressGenerator := &turn.RelayAddressGeneratorStatic{
		RelayAddress: net.ParseIP(publicIP), // Claim that we are listening on IP passed by user
		Address:      "0.0.0.0",             // But actually be listening on every interface
	}

	packetConnConfigs := make([]turn.PacketConnConfig, threadNum)
	for i := 0; i < threadNum; i++ {
		conn, listErr := listenerConfig.ListenPacket(ctx, addr.Network(), addr.String())
		if listErr != nil {
			log.Fatalf("Failed to allocate UDP listener at %s:%s", addr.Network(), addr.String())
		}

		packetConnConfigs[i] = turn.PacketConnConfig{
			PacketConn:            conn,
			RelayAddressGenerator: relayAddressGenerator,
		}

		log.Printf("Server %d listening on %s\n", i, conn.LocalAddr().String())
	}

	s, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		// Set AuthHandler callback
		// This is called every time a user tries to authenticate with the TURN server
		// Return the key for that user, or false when no user is found
		AuthHandler: func(username string, realm string, srcAddr net.Addr) ([]byte, bool) {
			if key, ok := usersMap[username]; ok {
				return key, true
			}
			return nil, false
		},
		// PacketConnConfigs is a list of UDP Listeners and the configuration around them
		PacketConnConfigs: packetConnConfigs,
	})
	if err != nil {
		log.Panicf("Failed to create TURN server: %s", err)
	}

	return s
}
