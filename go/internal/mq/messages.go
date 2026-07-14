package mq

// EventMsg flows from ingest to the fan-out consumer. It intentionally carries
// only the id — the payload is read from Postgres, the system of record.
type EventMsg struct {
	EventID string `json:"event_id"`
}

// DeliveryMsg represents one delivery attempt to one endpoint. Attempt is the
// 1-based number of the attempt this message authorizes; it is what makes
// attempt recording idempotent under redelivery.
type DeliveryMsg struct {
	DeliveryID string `json:"delivery_id"`
	Attempt    int    `json:"attempt"`
}

// DeadMsg is what lands in the DLQ after the retry budget is exhausted.
type DeadMsg struct {
	DeliveryID string `json:"delivery_id"`
	EventID    string `json:"event_id"`
	EndpointID string `json:"endpoint_id"`
	Attempts   int    `json:"attempts"`
	LastError  string `json:"last_error"`
}
