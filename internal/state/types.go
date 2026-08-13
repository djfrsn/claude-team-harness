package state

import "time"

type Scope struct {
	Key           string
	RoomID        string
	RootMessageID string
}

func RoomScope(roomID string) Scope {
	return Scope{Key: "room:" + roomID, RoomID: roomID}
}

func ThreadScope(roomID, rootMessageID string) Scope {
	return Scope{
		Key: "thread:" + roomID + ":" + rootMessageID, RoomID: roomID,
		RootMessageID: rootMessageID,
	}
}

type Conversation struct {
	Scope          Scope
	ACPSessionID   string
	Generation     int
	ContextEpoch   int
	CompletedTurns int
	Handoff        string
	UpdatedAt      time.Time
}

type Message struct {
	ID           string
	Scope        Scope
	Role         string
	SenderID     string
	Text         string
	InReplyTo    string
	ContextEpoch int
	CreatedAt    time.Time
}

type WebexEvent struct {
	MessageID string
	RoomID    string
	Attempts  int
	CreatedAt time.Time
}

type MessageRun struct {
	ID            string
	Scope         Scope
	MessageID     string
	SenderID      string
	Text          string
	Mode          string
	Persona       string
	Status        string
	ResultPersona string
	ActiveRunID   string
	Reply         string
	StopReason    string
	Generation    int
	Cached        bool
	Steered       bool
	Error         string
	CreatedAt     time.Time
	StartedAt     time.Time
	CompletedAt   time.Time
}
