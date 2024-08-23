package sfu

import "fmt"

type RecorderExtention struct{}

func NewRecorderExtension() IManagerExtension {
	return &RecorderExtention{}
}

func (r *RecorderExtention) OnGetRoom(manager *Manager, roomID string) (*Room, error) {
	return nil, nil
}
func (r *RecorderExtention) OnBeforeNewRoom(id, name, roomType string) error {
	return nil
}
func (r *RecorderExtention) OnNewRoom(manager *Manager, room *Room) {
}

func (r *RecorderExtention) OnRoomClosed(manager *Manager, room *Room) {
	fmt.Println("room closed", room.SendCloseDatagram())
}
