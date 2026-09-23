package config

import "time"

type Config struct {
	ListenAddr       string
	Backends         []string
	HealthCheckEvery time.Duration
}