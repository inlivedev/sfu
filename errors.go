package sfu

import "errors"

var (
	ErrClientNotFound = errors.New("client not found")
	ErrClientExists   = errors.New("client already exists")

	ErrRoomIsClosed   = errors.New("room is closed")
	ErrRoomIsNotEmpty = errors.New("room is not empty")
	ErrDecodingData   = errors.New("error decoding data")
	ErrEncodingData   = errors.New("error encoding data")
	ErrNotFound       = errors.New("not found")
)
