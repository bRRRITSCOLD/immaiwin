package sandbox

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// Breakpoint describes a line breakpoint with optional condition.
type Breakpoint struct {
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

// DebugRequest extends RunRequest with debugging parameters.
type DebugRequest struct {
	RunRequest
	Breakpoints []Breakpoint `json:"breakpoints"`
}

// DebugSession tracks an active debug container.
type DebugSession struct {
	ID          string    `json:"id"`
	ContainerID string    `json:"container_id"`
	Language    Language  `json:"language"`
	DebugPort   int       `json:"debug_port"`
	Status      string    `json:"status"` // "starting", "running", "paused", "terminated"
	CreatedAt   time.Time `json:"created_at"`
}

// PortAllocator manages a fixed range of debug ports.
type PortAllocator struct {
	mu    sync.Mutex
	start int
	end   int
	used  map[int]bool
}

// NewPortAllocator creates a port allocator for the given range [start, end).
func NewPortAllocator(start, end int) *PortAllocator {
	return &PortAllocator{
		start: start,
		end:   end,
		used:  make(map[int]bool),
	}
}

// Acquire returns the next available port, verifying it's actually free on the host.
func (pa *PortAllocator) Acquire() (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	for p := pa.start; p < pa.end; p++ {
		if !pa.used[p] && isPortFree(p) {
			pa.used[p] = true
			return p, nil
		}
	}
	return 0, fmt.Errorf("sandbox: no debug ports available in range %d-%d", pa.start, pa.end)
}

// isPortFree checks if a port is available by attempting to listen on it.
func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Release frees a previously acquired port.
func (pa *PortAllocator) Release(port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.used, port)
}
