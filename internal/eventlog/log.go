package eventlog

import (
	"context"
	"database/sql"
	"time"
)

// Log is the central event store interface.
// All appends go through Atomic; Subscribe and Read are for consumers.
type Log interface {
	// Atomic: business变更+event append同事务。
	// err==nil时返回的Envelope已落盘且已唤醒订阅者；err!=nil时返回nil。
	Atomic(ctx context.Context, fn func(uow UnitOfWork) error) ([]Envelope, error)

	// Subscribe: 进程内订阅。SinceLive仅live；SinceBeginning从头replay；
	// N>0 从 seq>N 起 replay 衔接 live。
	Subscribe(ctx context.Context, opts SubscribeOptions) (Subscription, error)

	// Read: 按 partition + since 拉取一段历史，HTTP pull 用。
	Read(ctx context.Context, partition string, since Since, limit int) (ReadResult, error)

	// ReadAll: 跨 partition 拉取，仅 admin / replay 工具用。
	ReadAll(ctx context.Context, since Since, limit int) (ReadResult, error)

	// LatestSeq: 当前最大 seq。
	LatestSeq(ctx context.Context) (int64, error)

	// Checkpoints: projector checkpoint 子接口。
	Checkpoints() CheckpointStore
}

// Since selects the starting point for a subscription or read.
type Since int64

const (
	// SinceLive subscribes only to new events after subscription time.
	SinceLive Since = -1
	// SinceBeginning reads from seq=0.
	SinceBeginning Since = 0
)

// SubscribeOptions configures a subscription.
type SubscribeOptions struct {
	Partitions []string        // 空 = 订阅所有（仅 admin）
	Types      []string        // 空 = 订阅所有
	Since      Since           // -1=live only, 0=from beginning, N>0=replay from N
	BufferSize int             // 默认 256
	OnLag      func(lag int64) // 慢消费者告警钩子
}

// Subscription delivers events to a consumer.
type Subscription interface {
	C() <-chan Envelope // 事件 channel；close 表示订阅终止
	Cursor() int64      // 当前消费到的 seq
	Close() error
	Lag() int64 // 当前落后 LatestSeq 的事件数
}

// ReadResult is the result of a read operation.
type ReadResult struct {
	Events    []Envelope
	NextSince Since // 下次拉取应传入的 since
	HasMore   bool
}

// EnvelopeDraft is the input type for building an envelope before append.
// Payload is marshalled to JSON inside Append; pass a struct or map.
type EnvelopeDraft struct {
	Partition string
	Type      string
	Version   int
	Category  string
	Payload   any
	TraceID   string
	SpanID    string
	Actor     *Actor // optional; copied verbatim into Envelope.Actor
}

// Since returns the NextSince value to use for the next read.
func (r ReadResult) Since() Since { return r.NextSince }

// DB returns the underlying *sql.DB for the Log implementation.
// It is only valid when the Log is a *SQLiteLog.
func DB(l Log) *sql.DB {
	if sl, ok := l.(*SQLiteLog); ok {
		return sl.db
	}
	return nil
}

// Time returns the current wall-clock time as an RFC3339Nano string.
// Extracted so tests can substitute a fake clock.
var Time = func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
