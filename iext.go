package sfu

type IManagerExtension interface {
	// only the first extension that returns a room or an error will be used, once a room or an error is returned, the rest of the extensions will be ignored
	OnGetRoom(manager *Manager, roomID string) (*Room, error)
	OnNewRoom(manager *Manager, room *Room)
}

type IExtension interface {
	OnClientAdded(client *Client)
}
