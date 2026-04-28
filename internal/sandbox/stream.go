package sandbox

// StreamWriter implements io.Writer and sends each write as an OutputEvent on
// the given channel. Used by both Docker and k3s backends to relay live
// stdout/stderr to callers of StreamRun.
type StreamWriter struct {
	Stream string
	Ch     chan<- OutputEvent
}

func (w *StreamWriter) Write(p []byte) (int, error) {
	w.Ch <- OutputEvent{Stream: w.Stream, Data: string(p)}
	return len(p), nil
}
