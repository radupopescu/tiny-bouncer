package systemone

// ConfigError reports missing or invalid configuration for a system-one
// backend (a malformed threshold override, an unknown threshold key). Every
// backend's configuration errors share this one type so callers branch on a
// single identity; it is non-retryable by construction.
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }
