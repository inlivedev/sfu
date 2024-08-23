package sfu

type ClientLeftExtension struct{}

func NewClientLeftExtension() IExtension {
	return &ClientLeftExtension{}
}

func (r *ClientLeftExtension) OnBeforeClientAdded(room *Room, clientId string) error {
	return nil
}
func (r *ClientLeftExtension) OnClientAdded(room *Room, client *Client) {}
func (r *ClientLeftExtension) OnClientRemoved(room *Room, client *Client) {
	if room.sfu.clients.Length() == 0 {
		room.Close()
	}
}
