package metrics

// Metric is the interface all metric types implement.
// Metrics auto-register with the global registry on creation.
type Metric interface {
	Name() string
	Snapshot() map[string]interface{}
}
