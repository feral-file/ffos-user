package offlinecache

// DrainForTest blocks until every job in flight has finished, logging
// included. Tests cancel the materializer's base context and then drain, so a
// canceled job never logs through a test logger after its test has returned
// (the race detector reports that write against testing.T).
func (m *DeviceLocalMaterializer) DrainForTest() {
	m.mu.Lock()
	jobs := make([]*deviceLocalJob, 0, len(m.inflight))
	for _, job := range m.inflight {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	for _, job := range jobs {
		<-job.done
	}
}
