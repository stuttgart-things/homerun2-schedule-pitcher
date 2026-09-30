package models

type PitchResponse struct {
	ObjectID string `json:"objectId"`
	StreamID string `json:"streamId"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}
