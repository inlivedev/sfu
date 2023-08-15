package sfu

type IManagerExtension interface {
	// only the first extension that returns a room or an error will be used, once a room or an error is returned, the rest of the extensions will be ignored
	OnGetRoom(manager *Manager, roomID string) (*Room, error)
	// this can be use for authentication before a room is created
	OnBeforeNewRoom(id, name, roomType string) error
	OnNewRoom(manager *Manager, room *Room)
	OnRoomClosed(manager *Manager, room *Room)
}

type IExtension interface {
	// This can be use for authentication before a client add to a room
	OnBeforeClientAdded(room *Room, clientID string) error
	OnClientAdded(room *Room, client *Client)
	OnClientRemoved(room *Room, client *Client)
}
