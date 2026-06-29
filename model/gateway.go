package model

import "time"

type Route struct {
	Path   string `json:"path" binding:"required"`
	Target string `json:"target" binding:"required"`
}

type ChangePortRequest struct {
	Port string `json:"port" binding:"required"`
}

type SSLConfigRequest struct {
	Enabled  bool   `json:"enabled"`
	Port     string `json:"port"`
	Domain   string `json:"domain"`
	CertType string `json:"cert_type"`
}

type SSLConfigResponse struct {
	Enabled        bool      `json:"enabled"`
	Port           string    `json:"port"`
	Domain         string    `json:"domain"`
	CertType       string    `json:"cert_type"`
	EffectiveTime  time.Time `json:"effective_time"`
	ExpirationTime time.Time `json:"expiration_time"`
}
