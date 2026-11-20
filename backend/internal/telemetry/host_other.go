//go:build !linux

package telemetry

// readHost is unavailable off Linux; the dashboard hides the host section.
func readHost() (hostRaw, bool) { return hostRaw{}, false }
