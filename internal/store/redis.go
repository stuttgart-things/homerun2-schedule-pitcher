package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// DefaultPrefix is the key prefix from the design (#1, section 4).
const DefaultPrefix = "homerun2-schedule-pitcher"

// Redis keeps state in Redis, so restarts and other replicas do not pitch
// again what was already pitched. Keys:
//
//	<prefix>:check:<id>:state    hash, one field per state field
//	<prefix>:check:<id>:history  stream, last HistoryLimit runs
//	<prefix>:lock:<id>           run lock with an owner token
type Redis struct {
	client redis.UniversalClient
	prefix string
	// owner identifies this process, so it only ever releases its own locks.
	owner string
}

// NewRedis wraps a go-redis client. prefix defaults to DefaultPrefix.
func NewRedis(client redis.UniversalClient, prefix string) *Redis {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Redis{client: client, prefix: prefix, owner: hex.EncodeToString(b)}
}

// Prefix returns the key prefix in use.
func (r *Redis) Prefix() string { return r.prefix }

func (r *Redis) stateKey(id string) string   { return r.prefix + ":check:" + id + ":state" }
func (r *Redis) historyKey(id string) string { return r.prefix + ":check:" + id + ":history" }
func (r *Redis) lockKey(id string) string    { return r.prefix + ":lock:" + id }

func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Get(ctx context.Context, id string) (state.State, error) {
	h, err := r.client.HGetAll(ctx, r.stateKey(id)).Result()
	if err != nil {
		return state.State{}, fmt.Errorf("reading state of %s: %w", id, err)
	}
	if len(h) == 0 {
		return state.New(id), nil
	}
	return decodeState(id, h)
}

func (r *Redis) Put(ctx context.Context, s state.State) error {
	key := r.stateKey(s.CheckID)
	pipe := r.client.TxPipeline()
	// Replace the whole hash, so fields that became empty do not linger.
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key, encodeState(s))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("writing state of %s: %w", s.CheckID, err)
	}
	return nil
}

func (r *Redis) AddHistory(ctx context.Context, id string, e HistoryEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: r.historyKey(id),
		MaxLen: HistoryLimit,
		Approx: true,
		Values: map[string]any{"entry": data},
	}).Err()
}

func (r *Redis) History(ctx context.Context, id string, limit int) ([]HistoryEntry, error) {
	msgs, err := r.client.XRevRangeN(ctx, r.historyKey(id), "+", "-", int64(limit)).Result()
	if err != nil {
		return nil, fmt.Errorf("reading history of %s: %w", id, err)
	}
	out := make([]HistoryEntry, 0, len(msgs))
	for _, m := range msgs {
		raw, _ := m.Values["entry"].(string)
		var e HistoryEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *Redis) TryLock(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	ok, err := r.client.SetNX(ctx, r.lockKey(id), r.owner, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("taking lock of %s: %w", id, err)
	}
	return ok, nil
}

// unlockScript deletes the lock only if this process still owns it: after the
// TTL ran out another replica may hold it.
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

func (r *Redis) Unlock(ctx context.Context, id string) error {
	err := unlockScript.Run(ctx, r.client, []string{r.lockKey(id)}, r.owner).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("releasing lock of %s: %w", id, err)
	}
	return nil
}

func encodeState(s state.State) map[string]any {
	return map[string]any{
		"band":             s.Band.String(),
		"lastRun":          encodeTime(s.LastRun),
		"lastOk":           encodeTime(s.LastOK),
		"expiry":           encodeTime(s.Expiry),
		"noExpiry":         strconv.FormatBool(s.NoExpiry),
		"summary":          s.Summary,
		"subject":          s.Subject,
		"problem":          s.Problem,
		"failing":          strconv.FormatBool(s.Failing),
		"failingSince":     encodeTime(s.FailingSince),
		"lastError":        s.LastError,
		"pitchedBand":      s.PitchedBand.String(),
		"pitchedAt":        encodeTime(s.PitchedAt),
		"failPitchedAt":    encodeTime(s.FailPitchedAt),
		"noExpiryNotified": strconv.FormatBool(s.NoExpiryNotified),
	}
}

func decodeState(id string, h map[string]string) (state.State, error) {
	s := state.State{
		CheckID:          id,
		Band:             status.Parse(h["band"]),
		NoExpiry:         h["noExpiry"] == "true",
		Summary:          h["summary"],
		Subject:          h["subject"],
		Problem:          h["problem"],
		Failing:          h["failing"] == "true",
		LastError:        h["lastError"],
		PitchedBand:      status.Parse(h["pitchedBand"]),
		NoExpiryNotified: h["noExpiryNotified"] == "true",
	}
	// An unknown pitched band would re-pitch; ok means nothing is open.
	if s.PitchedBand == status.Unknown {
		s.PitchedBand = status.OK
	}
	var err error
	for field, dst := range map[string]*time.Time{
		"lastRun": &s.LastRun, "lastOk": &s.LastOK, "expiry": &s.Expiry,
		"failingSince": &s.FailingSince, "pitchedAt": &s.PitchedAt, "failPitchedAt": &s.FailPitchedAt,
	} {
		if *dst, err = decodeTime(h[field]); err != nil {
			return state.State{}, fmt.Errorf("state of %s: field %s: %w", id, field, err)
		}
	}
	return s, nil
}

func encodeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func decodeTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
