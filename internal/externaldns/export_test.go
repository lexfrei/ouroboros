package externaldns

// LastLogSize exposes the StatusSurfacer's dedupe-map size to the test
// package so the leak-prevention behaviour can be asserted directly
// instead of through inferred log output.
func LastLogSize(surfacer *StatusSurfacer) int {
	surfacer.mu.Lock()
	defer surfacer.mu.Unlock()

	return len(surfacer.lastLog)
}
