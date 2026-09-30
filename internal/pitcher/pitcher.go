package pitcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	homerun "github.com/stuttgart-things/homerun-library/v3"
)

// Pitcher defines the interface for message delivery backends.
type Pitcher interface {
	Pitch(msg homerun.Message) (objectID, streamID string, err error)
}

// RedisPitcher enqueues messages into Redis Streams.
type RedisPitcher struct {
	Config homerun.RedisConfig
}

// HealthCheck pings Redis to verify connectivity.
func (p *RedisPitcher) HealthCheck(ctx context.Context) error {
	client := redis.NewClient(&redis.Options{
		Addr:     p.Config.Addr + ":" + p.Config.Port,
		Password: p.Config.Password,
	})
	defer func() { _ = client.Close() }()

	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping failed: %w", err)
	}
	return nil
}

func (p *RedisPitcher) Pitch(msg homerun.Message) (string, string, error) {
	objectID, streamID, err := homerun.EnqueueMessageInRedisStreams(msg, p.Config)
	if err != nil {
		return "", "", fmt.Errorf("failed to enqueue message to Redis stream: %w", err)
	}
	return objectID, streamID, nil
}

// FilePitcher writes messages as JSON lines to a file (dev/testing mode).
type FilePitcher struct {
	Path string
	mu   sync.Mutex
}

func (p *FilePitcher) Pitch(msg homerun.Message) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	f, err := os.OpenFile(p.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", "", fmt.Errorf("failed to open pitch file: %w", err)
	}
	defer func() { _ = f.Close() }()

	objectID := fmt.Sprintf("file-%d", time.Now().UnixNano())
	streamID := "file"

	entry := map[string]any{
		"objectID":  objectID,
		"streamID":  streamID,
		"timestamp": time.Now().Format(time.RFC3339),
		"message":   msg,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal message: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return "", "", fmt.Errorf("failed to write to pitch file: %w", err)
	}

	slog.Debug("message pitched to file", "objectID", objectID, "path", p.Path)
	return objectID, streamID, nil
}
