package sfu

import (
	"context"
	"errors"
	"sync"

	"github.com/pion/webrtc/v3"
)

var (
	ErrQueueIsClosed = errors.New("queue is closed")
)

type queue struct {
	opChan chan interface{}
	mutex  sync.Mutex
	IsOpen bool
}

type negotiationQueue struct {
	Client     *Client
	SDP        webrtc.SessionDescription
	AnswerChan chan webrtc.SessionDescription
	ErrorChan  chan error
}

type renegotiateQueue struct {
	Client *Client
}

type allowRemoteRenegotiationQueue struct {
	Client *Client
}

func NewQueue(ctx context.Context) *queue {
	q := &queue{
		IsOpen: true,
		mutex:  sync.Mutex{},
		opChan: make(chan interface{}, 10),
	}

	go q.run(ctx)

	return q
}

func (q *queue) Push(item interface{}) {
	go func() {
		q.mutex.Lock()
		defer q.mutex.Unlock()
		if !q.IsOpen {
			return
		}

		q.opChan <- item
	}()
}

func (q *queue) run(ctx context.Context) {
	ctxx, cancel := context.WithCancel(ctx)
	defer func() {
		q.mutex.Lock()
		cancel()
		q.IsOpen = false
		close(q.opChan)
		q.mutex.Unlock()
	}()

	for {
		select {
		case <-ctxx.Done():
			return
		case item := <-q.opChan:
			switch opItem := item.(type) {
			case negotiationQueue:
				answer, err := opItem.Client.negotiateQueuOp(opItem.SDP)
				if err != nil {
					opItem.ErrorChan <- err
					continue
				}

				opItem.AnswerChan <- *answer
			case renegotiateQueue:
				opItem.Client.renegotiateQueuOp()
			case allowRemoteRenegotiationQueue:
				opItem.Client.allowRemoteRenegotiationQueuOp()
			}
		}
	}
}
