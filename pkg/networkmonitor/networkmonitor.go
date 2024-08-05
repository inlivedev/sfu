package networkmonitor

import (
	"context"
	"time"
)

type NetworkConditionType uint8

const (
	uint16SizeHalf = uint16(1 << 15)
	RECEIVENORMAL  = NetworkConditionType(1)
	RECEIVELOSS    = NetworkConditionType(2)
	SENDERNORMAL   = NetworkConditionType(3)
	SENDERLOSS     = NetworkConditionType(4)
)

type NetworkMonitor struct {
	context                           context.Context
	receiverCondition                 NetworkConditionType
	senderCondition                   NetworkConditionType
	consecutiveConditionToChangeState uint8
	consecutiveConditionCount         uint8
}

func New(ctx context.Context, interval time.Duration, consecutiveConditionToChangeState uint8) *NetworkMonitor {
	return &NetworkMonitor{
		context:                           ctx,
		consecutiveConditionToChangeState: consecutiveConditionToChangeState,
	}
}
