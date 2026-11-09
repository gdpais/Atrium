// Package config is retained as a compatibility shim for db-collector callers.
// New code should import atrium/internal/config directly.
package config

import shared "atrium/internal/config"

type Endpoint = shared.Endpoint
type NotificationChannel = shared.NotificationChannel
type CollectorRuntimeConfig = shared.CollectorRuntimeConfig
type TargetRuntimeConfig = shared.TargetRuntimeConfig
type ProbeRuntimeConfig = shared.ProbeRuntimeConfig
type RuntimeConfig = shared.RuntimeConfig

var LoadRuntimeConfig = shared.LoadRuntimeConfig
