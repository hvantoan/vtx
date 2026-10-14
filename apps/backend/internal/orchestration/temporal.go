package orchestration

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/client"
)

type TemporalClient struct {
	Client client.Client
}

func NewTemporalClient(hostPort, namespace string) (*TemporalClient, error) {
	c, err := client.Dial(client.Options{
		HostPort:  hostPort,
		Namespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to dial temporal server: %w", err)
	}

	return &TemporalClient{Client: c}, nil
}

func (t *TemporalClient) Ping(ctx context.Context) (time.Duration, error) {
	if t == nil || t.Client == nil {
		return 0, fmt.Errorf("temporal client is nil")
	}

	start := time.Now()
	_, err := t.Client.CheckHealth(ctx, &client.CheckHealthRequest{})
	latency := time.Since(start)

	if err != nil {
		return latency, fmt.Errorf("temporal health check failed: %w", err)
	}

	return latency, nil
}

func (t *TemporalClient) Close() {
	if t != nil && t.Client != nil {
		t.Client.Close()
	}
}
