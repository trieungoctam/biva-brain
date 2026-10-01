// Package scheduler chạy việc định kỳ trên đúng MỘT instance brain-api (leader).
//
// Leader giữ pg_try_advisory_lock trên một kết nối riêng; kết nối đứt thì Postgres tự nhả lock
// và instance khác lên thay ở lượt thử kế tiếp. Việc hiện có: trả job hết lease về hàng đợi.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey: khoá advisory của leader scheduler (hằng số riêng của BIVA Brain).
const lockKey int64 = 0x0B1A_B4A1

type Task struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context, db *pgxpool.Pool) error
}

type Scheduler struct {
	DB         *pgxpool.Pool
	Tasks      []Task
	RetryEvery time.Duration // chu kỳ thử giành quyền leader; 0 = 10s

	// OnLeader (tuỳ chọn) được gọi khi trở thành / mất leader; dùng cho test và metric.
	OnLeader func(bool)
}

// DefaultTasks là việc định kỳ của M0.
func DefaultTasks() []Task {
	return []Task{{Name: "requeue_expired", Every: 15 * time.Second, Run: RequeueExpired}}
}

// RequeueExpired trả job hết lease về hàng đợi (logic nằm trong SQL, migration 000002).
func RequeueExpired(ctx context.Context, db *pgxpool.Pool) error {
	var n int
	if err := db.QueryRow(ctx, `SELECT operations_requeue_expired()`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		slog.Warn("trả job hết lease về hàng đợi", "count", n)
	}
	return nil
}

// Run chặn tới khi ctx bị huỷ.
func (s *Scheduler) Run(ctx context.Context) {
	retry := s.RetryEvery
	if retry == 0 {
		retry = 10 * time.Second
	}
	for {
		s.lead(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// lead giành lock; nếu được thì chạy task tới khi mất kết nối hoặc ctx huỷ.
func (s *Scheduler) lead(ctx context.Context) {
	pc, err := s.DB.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: không lấy được kết nối", "err", err)
		}
		return
	}
	// Tách khỏi pool: lock gắn với session, không được trả kết nối đang giữ lock về pool.
	conn := pc.Hijack()
	defer conn.Close(context.Background())

	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil || !got {
		return
	}
	slog.Info("scheduler: trở thành leader")
	s.setLeader(true)
	defer s.setLeader(false)

	ticks := make([]*time.Ticker, len(s.Tasks))
	cases := make(chan int)
	for i, t := range s.Tasks {
		ticks[i] = time.NewTicker(t.Every)
		defer ticks[i].Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticks[i].C:
					select {
					case cases <- i:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	alive := time.NewTicker(5 * time.Second)
	defer alive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-alive.C:
			if err := conn.Ping(ctx); err != nil {
				slog.Warn("scheduler: mất kết nối giữ lock, thôi làm leader", "err", err)
				return
			}
		case i := <-cases:
			t := s.Tasks[i]
			if err := t.Run(ctx, s.DB); err != nil && ctx.Err() == nil {
				slog.Error("scheduler: task lỗi", "task", t.Name, "err", err)
			}
		}
	}
}

func (s *Scheduler) setLeader(v bool) {
	if s.OnLeader != nil {
		s.OnLeader(v)
	}
}
