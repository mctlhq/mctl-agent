package webhook

import "time"

type EventType string

const (
	EventTicketCreated   EventType = "ticket.created"
	EventTicketFixFailed EventType = "ticket.fix_failed"
	EventTicketEscalated EventType = "ticket.escalated"
)

var SupportedEventTypes = map[EventType]struct{}{
	EventTicketCreated:   {},
	EventTicketFixFailed: {},
	EventTicketEscalated: {},
}

type ResultStatus string

const (
	ResultAccepted   ResultStatus = "accepted"
	ResultPRCreated  ResultStatus = "pr_created"
	ResultNeedsHuman ResultStatus = "needs_human"
	ResultDeclined   ResultStatus = "declined"
	ResultFailed     ResultStatus = "failed"
)

type ClaimStatus string

const (
	ClaimActive    ClaimStatus = "active"
	ClaimExpired   ClaimStatus = "expired"
	ClaimCompleted ClaimStatus = "completed"
	ClaimRejected  ClaimStatus = "rejected"
)

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryFailed    DeliveryStatus = "failed"
	DeliveryDead      DeliveryStatus = "dead"
)

type TicketPayload struct {
	ID         string `json:"id"`
	Team       string `json:"team"`
	Service    string `json:"service"`
	Type       string `json:"type"`
	Severity   string `json:"severity"`
	Status     string `json:"status"`
	Summary    string `json:"summary"`
	Analysis   string `json:"analysis,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

type DiagnosisPayload struct {
	Diagnosis  string `json:"diagnosis"`
	Confidence string `json:"confidence"`
	Fixable    bool   `json:"fixable"`
	FixType    string `json:"fix_type,omitempty"`
}

type DeliveryInfo struct {
	ClaimURL        string `json:"claim_url"`
	ResultURL       string `json:"result_url"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds"`
}

type Event struct {
	ID         string            `json:"event_id"`
	Type       EventType         `json:"event_type"`
	OccurredAt time.Time         `json:"occurred_at"`
	Ticket     TicketPayload     `json:"ticket"`
	Diagnosis  *DiagnosisPayload `json:"diagnosis,omitempty"`
	Delivery   DeliveryInfo      `json:"delivery"`
}

type WebhookEndpoint struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	URL        string    `json:"url"`
	Secret     string    `json:"secret,omitempty"`
	EventTypes []string  `json:"event_types"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type ExternalDelivery struct {
	ID                    string         `json:"id"`
	EventID               string         `json:"event_id"`
	WebhookID             string         `json:"webhook_id"`
	Attempt               int            `json:"attempt"`
	Status                DeliveryStatus `json:"status"`
	ResponseCode          int            `json:"response_code"`
	ResponseBodyTruncated string         `json:"response_body_truncated,omitempty"`
	LastError             string         `json:"last_error,omitempty"`
	NextAttemptAt         time.Time      `json:"next_attempt_at"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

type ClaimRequest struct {
	AgentID string `json:"agent_id"`
	EventID string `json:"event_id"`
}

type ClaimResponse struct {
	LeaseID   string    `json:"lease_id"`
	TicketID  string    `json:"ticket_id"`
	EventID   string    `json:"event_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ExternalClaim struct {
	ID             string       `json:"id"`
	EventID        string       `json:"event_id"`
	TicketID       string       `json:"ticket_id"`
	AgentID        string       `json:"agent_id"`
	Status         ClaimStatus  `json:"status"`
	LeaseExpiresAt time.Time    `json:"lease_expires_at"`
	ResultStatus   ResultStatus `json:"result_status,omitempty"`
	ResultPayload  string       `json:"result_payload,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	CompletedAt    *time.Time   `json:"completed_at,omitempty"`
}

type ExternalResult struct {
	AgentID         string            `json:"agent_id"`
	EventID         string            `json:"event_id"`
	LeaseID         string            `json:"lease_id"`
	IdempotencyKey  string            `json:"idempotency_key"`
	Status          ResultStatus      `json:"status"`
	Summary         string            `json:"summary"`
	Artifacts       map[string]string `json:"artifacts,omitempty"`
	MessageTemplate string            `json:"message_template,omitempty"`
}
