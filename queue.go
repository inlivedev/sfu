package sfu

import (
	"context"

	"github.com/pion/webrtc/v3"
)

type queue struct {
	opChan chan interface{}
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
		opChan: make(chan interface{}, 10),
	}

	go q.run(ctx)

	return q
}

func (q *queue) Push(item interface{}) {
	go func() {
		q.opChan <- item
	}()
}

func (q *queue) run(ctx context.Context) {
	ctxx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		close(q.opChan)
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
