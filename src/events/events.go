package events

import (
	"time"
)

type Event struct {
	UserID          string      `json:"user_id"`
	EventType       string      `json:"event_type"`
	Timestamp       time.Time   `json:"timestamp"`
	Location        string      `json:"location"`
	IP              string      `json:"ip"`
	Status          string      `json:"status"`
	FailedAttempts  int         `json:"failed_attempts"`
	DataTransferred int         `json:"data_transferred"`
	GeoLocation     GeoLocation `json:"geo_location"`
	User            User        `json:"user"`
}

type GeoLocation struct {
	Country string `json:"country"`
	City    string `json:"city"`
}

type User struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Email    string `json:"email"`
}
