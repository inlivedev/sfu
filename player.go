package sfu

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

type AudioPlayerState string
type PacketType string

const (
	AudioPlayerStatePlaying AudioPlayerState = "playing"
	AudioPlayerStatePaused  AudioPlayerState = "paused"
	AudioPlayerStateStopped AudioPlayerState = "stopped"
)

type AudioPlayer struct {
	track      *webrtc.TrackLocalStaticSample
	state      AudioPlayerState
	mu         sync.Mutex
	cancelFunc context.CancelFunc
	ctx        context.Context
	done       chan bool
}

func newAudioPlayer(t *webrtc.TrackLocalStaticSample, done chan bool) *AudioPlayer {
	ctx, cancle := context.WithCancel(context.Background())
	return &AudioPlayer{
		track:      t,
		state:      AudioPlayerStatePaused,
		mu:         sync.Mutex{},
		cancelFunc: cancle,
		ctx:        ctx,
		done:       done,
	}
}

func (p *AudioPlayer) changeState(newState AudioPlayerState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = newState
}

func (p *AudioPlayer) PlayURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download audio, status code: %d", resp.StatusCode)
	}
	return p.Play(resp.Body)
}

func (p *AudioPlayer) PlayFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	return p.Play(file)
}

func (p *AudioPlayer) Play(reader io.ReadCloser) error {
	p.mu.Lock()
	if p.state == AudioPlayerStatePlaying {
		p.mu.Unlock()
		return fmt.Errorf("audio player is already playing")
	}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFunc = cancel
	p.ctx = ctx
	go func() {
		//defer reader.Close()
		err := p.playStream(reader)
		if err != nil {
			fmt.Printf("playStream error: %v\n", err)
		}
	}()
	return nil
}

func (p *AudioPlayer) Pause() {
	p.changeState(AudioPlayerStatePaused)
}

func (p *AudioPlayer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != AudioPlayerStateStopped {
		if p.cancelFunc != nil {
			p.cancelFunc()
		}
		p.changeState(AudioPlayerStateStopped)
	}
}

func (p *AudioPlayer) playStream(reader io.ReadCloser) error {
	oggr, _, err := oggreader.NewWith(reader)
	if err != nil {
		return err
	}
	var lastGranule uint64
	ticker := time.NewTicker(oggPageDuration)
	defer ticker.Stop()
	for ; true; <-ticker.C {
		select {
		case <-p.ctx.Done():
			return nil

		default:
			pageData, pageHeader, oggErr := oggr.ParseNextPage()
			if oggErr == io.EOF {
				p.done <- true
				return nil
			}

			if oggErr != nil {
				return oggErr
			}

			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

			if oggErr = p.track.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); oggErr != nil {
				continue
			}
		}
	}

	p.changeState(AudioPlayerStatePlaying)
	return nil
}
