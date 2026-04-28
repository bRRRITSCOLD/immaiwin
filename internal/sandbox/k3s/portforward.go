package k3s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// portForwardSession is the runtime handle for an active port-forward.
// Closing stopCh triggers shutdown; readyCh fires once the local listener is bound.
type portForwardSession struct {
	stopCh  chan struct{}
	readyCh chan struct{}
}

// startPortForward opens a SPDY port-forward from localhost:localPort to
// podPort inside the named pod. Returns once the local listener is bound, or
// errors if the dialer cannot be constructed. Close session.stopCh to tear
// down the tunnel.
func (r *Runtime) startPortForward(ctx context.Context, podName string, localPort, podPort int) (*portForwardSession, error) {
	transport, upgrader, err := spdy.RoundTripperFor(r.restConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox/k3s: spdy roundtripper: %w", err)
	}

	url := r.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(r.ns).
		Name(podName).
		SubResource("portforward").
		URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})

	fw, err := portforward.New(
		dialer,
		[]string{fmt.Sprintf("%d:%d", localPort, podPort)},
		stopCh,
		readyCh,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox/k3s: portforward.New: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		// ForwardPorts blocks until stopCh closes or an error occurs.
		errCh <- fw.ForwardPorts()
	}()

	select {
	case <-readyCh:
		return &portForwardSession{stopCh: stopCh, readyCh: readyCh}, nil
	case err := <-errCh:
		return nil, fmt.Errorf("sandbox/k3s: portforward failed before ready: %w", err)
	case <-time.After(15 * time.Second):
		close(stopCh)
		return nil, fmt.Errorf("sandbox/k3s: portforward did not become ready within 15s")
	case <-ctx.Done():
		close(stopCh)
		return nil, ctx.Err()
	}
}
