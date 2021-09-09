package liveshare

import (
	"context"
	"fmt"
	"io"
	"net"
)

// A PortForwarder forwards TCP traffic over a LiveShare session from a port on a remote
// container to a local destination such as a network port or Go reader/writer.
type PortForwarder struct {
	session    *Session
	name       string
	remotePort int
}

// NewPortForwarder returns a new PortForwarder for the specified
// remote port and Live Share session. The name describes the purpose
// of the remote port or service.
func NewPortForwarder(session *Session, name string, remotePort int) *PortForwarder {
	return &PortForwarder{
		session:    session,
		name:       name,
		remotePort: remotePort,
	}
}

// ForwardToListener forwards traffic between the container's remote
// port and a local port, which must already be listening for
// connections. (Accepting a listener rather than a port number avoids
// races against other processes opening ports, and against a client
// connecting to the socket prematurely.)
//
// ForwardToListener accepts and handles connections on the local port
// until the context is cancelled. The caller is responsible for closing the listening port.
func (fwd *PortForwarder) ForwardToListener(ctx context.Context, listen net.Listener) (err error) {
	id, err := fwd.shareRemotePort(ctx)
	if err != nil {
		return err
	}

	errc := make(chan error, 1)
	sendError := func(err error) {
		// Use non-blocking send, to avoid goroutines getting
		// stuck in case of concurrent or sequential errors.
		select {
		case errc <- err:
		default:
		}
	}
	go func() {
		for {
			conn, err := listen.Accept()
			if err != nil {
				sendError(err)
				return
			}

			go func() {
				if err := fwd.handleConnection(ctx, id, conn); err != nil {
					sendError(err)
				}
			}()
		}
	}()

	return awaitError(ctx, errc)
}

// Forward forwards traffic between the container's remote port and
// the specified read/write stream. On return, the stream is closed.
func (fwd *PortForwarder) Forward(ctx context.Context, conn io.ReadWriteCloser) error {
	id, err := fwd.shareRemotePort(ctx)
	if err != nil {
		conn.Close()
		return err
	}

	// Create buffered channel so that send doesn't get stuck after context cancellation.
	errc := make(chan error, 1)
	go func() {
		if err := fwd.handleConnection(ctx, id, conn); err != nil {
			errc <- err
		}
	}()
	return awaitError(ctx, errc)
}

func (fwd *PortForwarder) shareRemotePort(ctx context.Context) (channelID, error) {
	id, err := fwd.session.startSharing(ctx, fwd.name, fwd.remotePort)
	if err != nil {
		err = fmt.Errorf("failed to share remote port %d: %v", fwd.remotePort, err)
	}
	return id, nil
}

func awaitError(ctx context.Context, errc <-chan error) error {
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err() // canceled
	}
}

// handleConnection handles forwarding for a single accepted connection, then closes it.
func (fwd *PortForwarder) handleConnection(ctx context.Context, id channelID, conn io.ReadWriteCloser) (err error) {
	defer safeClose(conn, &err)

	channel, err := fwd.session.openStreamingChannel(ctx, id)
	if err != nil {
		return fmt.Errorf("error opening streaming channel for new connection: %v", err)
	}
	// Ideally we would call safeClose again, but (*ssh.channel).Close
	// appears to have a bug that causes it return io.EOF spuriously
	// if its peer closed first; see github.com/golang/go/issues/38115.
	defer func() {
		closeErr := channel.Close()
		if err == nil && closeErr != io.EOF {
			err = closeErr
		}
	}()

	// Bi-directional copy of data.
	// If any individual connection has an error, we can safely ignore them
	// and defer to connection clients to handle data loss as necessary.
	go io.Copy(conn, channel)
	go io.Copy(channel, conn)

	<-ctx.Done()
	return ctx.Err()
}

// safeClose reports the error (to *err) from closing the stream only
// if no other error was previously reported.
func safeClose(closer io.Closer, err *error) {
	closeErr := closer.Close()
	if *err == nil {
		*err = closeErr
	}
}
