# Room Package
Each room will have one SFU attached to the room. The SFU will be responsible for manage the clients in the room. And the room will be as a signaling controller between SFU and the clients.

SFU <-----> Room <-----> HTTP API

## Room
- When a room is created the SFU will be created and attached to the room.
- When a client is disconnected from the SFU, the room should check if there is any client left in the room, if not, the room should be closed.