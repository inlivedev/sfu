package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/testhelper"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestActiveTracks(t *testing.T) {
	t.Parallel()
	// _ = os.Setenv("PION_LOG_DEBUG", "pc,dtls")
	// _ = os.Setenv("PION_LOG_TRACE", "ice")
	// _ = os.Setenv("PIONS_LOG_INFO", "all")

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	peerCount := 3
	trackCount := 0
	connectedCount := 0
	trackChan := make(chan *RemoteTrack)
	remoteTracks := make(map[string]map[string]*webrtc.TrackRemote)
	peerChan := make(chan *PeerClient)
	connectedChan := make(chan bool)
	peers := make(map[string]*PeerClient, 0)
	udpMux := NewUDPMux(ctx, 40004)
	trackEndedChan := make(chan bool)

	// sfu := testhelper.Setup(t, udpMux, ctx, peerCount, trackChan, peerChan, connectedChan)
	sfu := Setup(t, ctx, udpMux, peerCount, trackChan, peerChan, connectedChan)
	defer sfu.Stop()

	ctxTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)

	defer cancel()

	expectedTracks := (peerCount * 2) * (peerCount - 1)
	glog.Info("expected tracks: ", expectedTracks)

	continueChan := make(chan bool)
	stoppedClient := 0

	isStopped := make(chan bool)
	trackEndedCount := 0

	go func() {
		for {
			select {
			case <-ctxTimeout.Done():
				require.Equal(t, expectedTracks, trackCount)
				return
			case <-connectedChan:
				connectedCount++
				glog.Info("connected count: ", connectedCount)
			case remoteTrack := <-trackChan:
				if _, ok := remoteTracks[remoteTrack.Client.ID]; !ok {
					remoteTracks[remoteTrack.Client.ID] = make(map[string]*webrtc.TrackRemote)
				}

				remoteTracks[remoteTrack.Client.ID][remoteTrack.Track.ID()] = remoteTrack.Track

				go func() {
					rtcpBuf := make([]byte, 1500)
					ctxx, cancell := context.WithCancel(ctx)
					defer cancell()

					for {
						select {
						case <-ctxx.Done():
							return
						default:
							if _, _, rtcpErr := remoteTrack.Track.Read(rtcpBuf); rtcpErr != nil {
								if rtcpErr.Error() == "EOF" {
									trackEndedChan <- true
									return
								}
								return
							}
						}
					}

				}()
				trackCount++
				glog.Info("track count: ", trackCount)

				if trackCount == expectedTracks { // 2 clients
					totalRemoteTracks := 0
					for _, clientTrack := range remoteTracks {
						for range clientTrack {
							totalRemoteTracks++
						}
					}

					glog.Info("total remote tracks: ", totalRemoteTracks)
					continueChan <- true
				}

			case client := <-peerChan:
				peers[client.ID] = client
				glog.Info("peer count: ", len(peers))
			case <-isStopped:
				stoppedClient++
				if stoppedClient == 1 {
					continueChan <- true
				}
			case <-trackEndedChan:
				trackEndedCount++
				glog.Info("track ended count: ", trackEndedCount)
			}
		}
	}()

	<-continueChan

	require.Equal(t, expectedTracks, trackCount)

	currentTrack := 0

	for _, client := range peers {
		glog.Info("client: ", client.ID, "remote track count: ", len(client.PeerConnection.GetReceivers()))
		for _, receiver := range client.PeerConnection.GetReceivers() {
			if receiver != nil && receiver.Track() != nil {
				currentTrack++
			}
		}
	}

	glog.Info("current clients count:", len(peers), ",current client tracks count:", currentTrack, "peer tracks count: ", trackCount)

	for _, client := range peers {
		relay, _ := sfu.GetClient(client.ID)

		relay.OnConnectionStateChanged(func(state webrtc.PeerConnectionState) {
			if state == webrtc.PeerConnectionStateClosed {
				isStopped <- true
			}
		})

		err := relay.Stop()
		require.NoError(t, err)
		delete(peers, client.ID)
		peerCount = len(peers)

		// stop after one client
		break
	}

	<-continueChan

	require.Equal(t, 1, stoppedClient)

	// count left tracks
	leftTracks := 0
	expectedLeftTracks := len(sfu.GetClients()) * 2 * (len(sfu.GetClients()))

	for _, client := range sfu.GetClients() {
		for _, receiver := range client.GetPeerConnection().GetReceivers() {
			if receiver.Track() != nil {
				leftTracks++
			}
		}
	}

	currentTrack = 0

	for _, peer := range peers {
		for _, transceiver := range peer.PeerConnection.GetTransceivers() {
			if transceiver != nil && transceiver.Receiver().Track() != nil {
				currentTrack++
			}
		}
	}

	glog.Info("current tracks count: ", currentTrack)

	glog.Info("left tracks: ", leftTracks, "from clients: ", len(sfu.GetClients()))
	glog.Info("expected left tracks: ", expectedLeftTracks)
	require.Equal(t, expectedLeftTracks, leftTracks)

	glog.Info("test adding extra tracks")
	// reset track count and expected tracks
	trackCount = 0
	expectedTracks = (peerCount * 2) * (peerCount - 1)

	// Test adding extra 2 tracks for each peer
	for _, peer := range peers {
		peer.InRenegotiation = true
		newTracks, _, _ := testhelper.GetStaticTracks(ctx, testhelper.GenerateSecureToken(16), true)

		// renegotiate after adding tracks
		allowRenegotiate := peer.RelayClient.IsAllowNegotiation()
		if allowRenegotiate {
			for _, track := range newTracks {
				_, err := peer.PeerConnection.AddTrack(track)
				require.NoError(t, err)
			}
			glog.Info("test: renegotiating", peer.ID)
			offer, _ := peer.PeerConnection.CreateOffer(nil)
			err := peer.PeerConnection.SetLocalDescription(offer)
			require.NoError(t, err)
			answer, err := peer.RelayClient.Negotiate(*peer.PeerConnection.LocalDescription())
			require.NoError(t, err)
			err = peer.PeerConnection.SetRemoteDescription(*answer)
			require.NoError(t, err)
		} else {
			glog.Info("not renegotiating", peer.ID)

			peer.PendingTracks = append(peer.PendingTracks, newTracks...)
		}

		peer.InRenegotiation = false
	}

	timeoutt, cancellTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancellTimeout()

	select {
	case <-timeoutt.Done():
		require.Fail(t, "timeout")
	case <-continueChan:
		require.Equal(t, expectedTracks, trackCount)
	}
}
