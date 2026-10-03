// Package scheduler chạy việc định kỳ trên đúng MỘT instance brain-api (leader).
//
// Leader giữ pg_try_advisory_lock trên một kết nối riêng; kết nối đứt thì Postgres tự nhả lock
// và instance khác lên thay ở lượt thử kế tiếp. Việc hiện có: trả job hết lease về hàng đợi.
package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strconv"
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
	return []Task{
		{Name: "requeue_expired", Every: 15 * time.Second, Run: RequeueExpired},
		{Name: "expire_items", Every: time.Minute, Run: ExpireItems},
		// Lịch sử job (done/failed) chỉ để chẩn đoán gần đây — promote enqueue job mỗi
		// 5 phút nên bảng operations lớn vô hạn nếu không dọn (đo live: 115 row/10h).
		{Name: "purge_operations", Every: time.Hour, Run: PurgeOperations},
		{Name: "expire_forms", Every: time.Hour, Run: ExpireForms},
	}
}

// PurgeOperations: xoá job kết thúc (done/failed) cũ hơn BIVA_OPERATIONS_RETENTION_DAYS
// (mặc định 30 ngày; 0 = tắt). audit_log là nhật ký lâu dài và KHÔNG bị dọn.
func PurgeOperations(ctx context.Context, db *pgxpool.Pool) error {
	days := 30
	if v := os.Getenv("BIVA_OPERATIONS_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	if days <= 0 {
		return nil
	}
	tag, err := db.Exec(ctx, `DELETE FROM operations
		WHERE status IN ('done', 'failed') AND created_at < now() - make_interval(days => $1)`, days)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n > 0 {
		slog.Info("scheduler: dọn lịch sử job cũ", "xoá", n, "giữ_lại_ngày", days)
	}
	return nil
}

// ExpireItems (S2.3.2): item active đã quá valid_to → expired. Trigger của migration 000011 đánh dấu artifact trích
// dẫn chúng là stale; trigger 000010 tăng version tri thức (knowledge pack dựng lại).
func ExpireItems(ctx context.Context, db *pgxpool.Pool) error {
	tag, err := db.Exec(ctx, `UPDATE items SET status = 'expired', updated_at = now()
		WHERE status = 'active' AND valid_to IS NOT NULL AND valid_to < now()`)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n > 0 {
		slog.Info("item hết hiệu lực → expired", "count", n)
	}
	return nil
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
	// Context con cho PHIÊN leader: khi mất lock/mất kết nối, mọi goroutine ticker của
	// phiên này thoát — dùng context của Scheduler khiến chúng sống sót và tích luỹ.
	leadCtx, cancelLead := context.WithCancel(ctx)
	defer cancelLead()

	ticks := make([]*time.Ticker, len(s.Tasks))
	cases := make(chan int)
	for i, t := range s.Tasks {
		ticks[i] = time.NewTicker(t.Every)
		defer ticks[i].Stop()
		go func() {
			for {
				select {
				case <-leadCtx.Done():
					return
				case <-ticks[i].C:
					select {
					case cases <- i:
					case <-leadCtx.Done():
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
		case <-leadCtx.Done():
			return
		case <-alive.C:
			if err := conn.Ping(leadCtx); err != nil {
				slog.Warn("scheduler: mất kết nối giữ lock, thôi làm leader", "err", err)
				return
			}
		case i := <-cases:
			t := s.Tasks[i]
			if err := t.Run(leadCtx, s.DB); err != nil && leadCtx.Err() == nil {
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

// ExpireForms: form hết hạn → 'closed' (status CHECK cho sẵn) — không chiếm slot cap
// MaxOpenForms và link /f/ trả 410 nhất quán (s5: từng chiếm slot vĩnh viễn).
func ExpireForms(ctx context.Context, db *pgxpool.Pool) error {
	tag, err := db.Exec(ctx, `UPDATE forms SET status = 'closed'
		WHERE status = 'open' AND expires_at < now()`)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n > 0 {
		slog.Info("scheduler: đóng form hết hạn", "số form", n)
	}
	return nil
}
