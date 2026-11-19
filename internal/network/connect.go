package network

import (
	"context"
	"errors"
	"io"
)

// Connect uses the caller's node identity to open one authorized stream.
func Connect(ctx context.Context, cfg *AgentConfig, destination string, input io.Reader, output io.Writer) error {
	_, service, ok := splitDestination(destination)
	if !ok || service == "webhook" {
		return errors.New("connect requires an ordinary service in node/service format")
	}
	agent, err := newAgent(cfg)
	if err != nil {
		return err
	}
	conn, err := agent.openWebSocket(ctx, transportPath(cfg.TransportPrefix, "/data"), "source", destination, "")
	if err != nil {
		return err
	}
	defer conn.Close()
	configureDataConn(conn)
	stop := make(chan struct{})
	defer close(stop)
	go pingLoop(conn, stop)
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	inputError := make(chan error, 1)
	go func() {
		if err := readerToWebSocket(conn, input); err != nil {
			inputError <- err
			_ = conn.Close()
		}
	}()
	err = webSocketToWriter(output, conn)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case err := <-inputError:
		return err
	default:
		return err
	}
}
