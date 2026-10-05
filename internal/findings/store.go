package findings

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Delivery remembers when office-hours messages were last sent.
type Delivery struct {
	LastUpdate     time.Time `json:"lastUpdate,omitzero"`
	LastStartOfDay time.Time `json:"lastStartOfDay,omitzero"`
	LastEndOfDay   time.Time `json:"lastEndOfDay,omitzero"`
}

// Store persists findings.
type Store interface {
	// List returns the findings of a source, or of all sources for "".
	List(ctx context.Context, source string) ([]Finding, error)
	Get(ctx context.Context, source, key string) (Finding, bool, error)
	Save(ctx context.Context, fs ...Finding) error
	Delete(ctx context.Context, fs ...Finding) error
	GetDelivery(ctx context.Context) (Delivery, error)
	PutDelivery(ctx context.Context, d Delivery) error
}

// Memory keeps findings in the process (no-Redis mode, tests).
type Memory struct {
	mu       sync.Mutex
	findings map[string]Finding
	delivery Delivery
}

func NewMemory() *Memory { return &Memory{findings: map[string]Finding{}} }

func (m *Memory) List(_ context.Context, source string) ([]Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Finding
	for _, f := range m.findings {
		if source == "" || f.Source == source {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *Memory) Get(_ context.Context, source, key string) (Finding, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.findings[source+"/"+key]
	return f, ok, nil
}

func (m *Memory) Save(_ context.Context, fs ...Finding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range fs {
		m.findings[f.ID()] = f
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, fs ...Finding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range fs {
		delete(m.findings, f.ID())
	}
	return nil
}

func (m *Memory) GetDelivery(context.Context) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delivery, nil
}

func (m *Memory) PutDelivery(_ context.Context, d Delivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivery = d
	return nil
}

// Redis stores each finding as a hash <prefix>:finding:<source>:<key>. The
// full finding is the JSON field "doc"; source, key, status, severity, host,
// tags and timestamps are also flat fields, so a RediSearch index
// (FT.CREATE ... ON HASH PREFIX 1 <prefix>:finding:) can be added later
// without migrating data. <prefix>:findings is the set of finding keys.
type Redis struct {
	client redis.UniversalClient
	prefix string
}

func NewRedis(client redis.UniversalClient, prefix string) *Redis {
	return &Redis{client: client, prefix: prefix}
}

func (r *Redis) key(source, key string) string {
	return r.prefix + ":finding:" + source + ":" + key
}
func (r *Redis) indexKey() string    { return r.prefix + ":findings" }
func (r *Redis) deliveryKey() string { return r.prefix + ":findings:delivery" }

func (r *Redis) List(ctx context.Context, source string) ([]Finding, error) {
	keys, err := r.client.SMembers(ctx, r.indexKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("listing findings: %w", err)
	}
	if source != "" {
		prefix := r.key(source, "")
		filtered := keys[:0]
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				filtered = append(filtered, k)
			}
		}
		keys = filtered
	}
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.HGet(ctx, k, "doc")
	}
	_, _ = pipe.Exec(ctx)
	var out []Finding
	for i, c := range cmds {
		doc, err := c.Result()
		if err == redis.Nil {
			// Index entry without document: drop it.
			r.client.SRem(ctx, r.indexKey(), keys[i])
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", keys[i], err)
		}
		var f Finding
		if err := json.Unmarshal([]byte(doc), &f); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", keys[i], err)
		}
		// A source that is a prefix of another ("dev" vs "dev-x") matches the
		// key prefix too; the document decides.
		if source == "" || f.Source == source {
			out = append(out, f)
		}
	}
	return out, nil
}

func (r *Redis) Get(ctx context.Context, source, key string) (Finding, bool, error) {
	doc, err := r.client.HGet(ctx, r.key(source, key), "doc").Result()
	if err == redis.Nil {
		return Finding{}, false, nil
	}
	if err != nil {
		return Finding{}, false, err
	}
	var f Finding
	if err := json.Unmarshal([]byte(doc), &f); err != nil {
		return Finding{}, false, err
	}
	return f, true, nil
}

func (r *Redis) Save(ctx context.Context, fs ...Finding) error {
	if len(fs) == 0 {
		return nil
	}
	pipe := r.client.TxPipeline()
	for _, f := range fs {
		doc, err := json.Marshal(f)
		if err != nil {
			return err
		}
		k := r.key(f.Source, f.Key)
		pipe.Del(ctx, k)
		pipe.HSet(ctx, k, map[string]any{
			"doc":         doc,
			"source":      f.Source,
			"key":         f.Key,
			"status":      f.Status,
			"severity":    f.Severity,
			"host":        f.Host,
			"tags":        strings.Join(f.Tags, ","),
			"first_seen":  unix(f.FirstSeen),
			"last_seen":   unix(f.LastSeen),
			"resolved_at": unix(f.ResolvedAt),
		})
		pipe.SAdd(ctx, r.indexKey(), k)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("saving findings: %w", err)
	}
	return nil
}

func (r *Redis) Delete(ctx context.Context, fs ...Finding) error {
	if len(fs) == 0 {
		return nil
	}
	pipe := r.client.TxPipeline()
	for _, f := range fs {
		k := r.key(f.Source, f.Key)
		pipe.Del(ctx, k)
		pipe.SRem(ctx, r.indexKey(), k)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Redis) GetDelivery(ctx context.Context) (Delivery, error) {
	doc, err := r.client.Get(ctx, r.deliveryKey()).Result()
	if err == redis.Nil {
		return Delivery{}, nil
	}
	if err != nil {
		return Delivery{}, err
	}
	var d Delivery
	err = json.Unmarshal([]byte(doc), &d)
	return d, err
}

func (r *Redis) PutDelivery(ctx context.Context, d Delivery) error {
	doc, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, r.deliveryKey(), doc, 0).Err()
}

func unix(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.Unix(), 10)
}
