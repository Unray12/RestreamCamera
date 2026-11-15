// Package model contains the domain types shared by the store and API layers.
package model

import "time"

// Source is a real camera / clip / publisher feeding the media server.
type Source struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`      // pull | publisher
	Transport string    `json:"transport"` // tcp | udp
	Enabled   bool      `json:"enabled"`
	Status    string    `json:"status"` // unknown | online | offline
	LastError string    `json:"lastError"`
	URLMasked string    `json:"urlMasked"`
	Path      string    `json:"path"`
	HasURL    bool      `json:"hasUrl"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// URL is write-only: it is accepted on create/update and never returned.
	URL string `json:"url,omitempty"`
	// URLEnc holds the AES-GCM ciphertext as stored in the database.
	URLEnc string `json:"-"`
}

// Camera is a virtual camera fanning out one source.
type Camera struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName,omitempty"`
	Group      string `json:"group"`
	Enabled    bool   `json:"enabled"`
	OnDemand   bool   `json:"onDemand"`
	MaxReaders int    `json:"maxReaders"`
	Mode       string `json:"mode"` // proxy | redirect
	ReadUser   string `json:"readUser"`
	ReadPass   string `json:"readPass,omitempty"` // write-only, or returned once on creation

	IPAllowlist string `json:"ipAllowlist"`
	Vendor      string `json:"vendor"`
	Model       string `json:"model"`
	Serial      string `json:"serial"`
	Firmware    string `json:"firmware"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Runtime   Runtime   `json:"runtime"`

	PassMasked string `json:"passMasked"`
	HasPass    bool   `json:"hasPass"`

	PassEnc string `json:"-"`
}

// Runtime is the live state of a camera path, refreshed by the reconciler.
type Runtime struct {
	State         string    `json:"state"` // unknown | ready | offline | disabled | error
	Readers       int       `json:"readers"`
	InKbps        int64     `json:"inKbps"`
	OutKbps       int64     `json:"outKbps"`
	InboundBytes  uint64    `json:"inboundBytes"`
	OutboundBytes uint64    `json:"outboundBytes"`
	LastError     string    `json:"lastError"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Overview is the aggregated dashboard state.
type Overview struct {
	CamerasTotal   int   `json:"camerasTotal"`
	CamerasEnabled int   `json:"camerasEnabled"`
	CamerasReady   int   `json:"camerasReady"`
	CamerasIdle    int   `json:"camerasIdle"`
	CamerasOffline int   `json:"camerasOffline"`
	SourcesTotal   int   `json:"sourcesTotal"`
	SourcesOnline  int   `json:"sourcesOnline"`
	Readers        int   `json:"readers"`
	InKbps         int64 `json:"inKbps"`
	OutKbps        int64 `json:"outKbps"`
	EventsLastHour int   `json:"eventsLastHour"`
}

// Event is an audit/activity record.
type Event struct {
	ID       int64     `json:"id"`
	TS       time.Time `json:"ts"`
	Level    string    `json:"level"`
	Category string    `json:"category"`
	Subject  string    `json:"subject"`
	Message  string    `json:"message"`
}

// Sample is one aggregate metrics sample (one per minute).
type Sample struct {
	TS       int64  `json:"ts"`
	Readers  int    `json:"readers"`
	InBytes  uint64 `json:"inBytes"`
	OutBytes uint64 `json:"outBytes"`
	Ready    int    `json:"ready"`
}

// CameraQuery filters camera listing.
type CameraQuery struct {
	Q        string
	Group    string
	SourceID string
	State    string
	Enabled  *bool
	Limit    int
	Offset   int
}
