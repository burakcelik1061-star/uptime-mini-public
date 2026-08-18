package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Bu dosyadaki testler SAHTE bağımlılıklarla koşuyor: ölçülen şeyler
// eşzamanlılık sınırı, kapanma davranışı ve hata yollarının hangi sayaca/
// yazmaya karşılık geldiği. Claim'in ATOMİKLİĞİ sahte store ile ölçülemez —
// o testler gerçek veritabanına karşı (claim_test.go).

type fakeStore struct {
	mu sync.Mutex

	// batches, ClaimDueMonitors çağrılarının sırayla döndüreceği partiler.
	batches  [][]store.Monitor
	claims   []int32
	claimErr error

	applied  []store.ApplyCheckOutcomeParams
	applyErr error
	released []uuid.UUID
}

func (s *fakeStore) ClaimDueMonitors(_ context.Context, rowLimit int32) ([]store.Monitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.claims = append(s.claims, rowLimit)

	if s.claimErr != nil {
		return nil, s.claimErr
	}

	if len(s.batches) == 0 {
		return nil, nil
	}

	batch := s.batches[0]
	s.batches = s.batches[1:]

	return batch, nil
}

func (s *fakeStore) ApplyCheckOutcome(_ context.Context, arg store.ApplyCheckOutcomeParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.applyErr != nil {
		return s.applyErr
	}

	s.applied = append(s.applied, arg)

	return nil
}

func (s *fakeStore) ReleaseMonitorClaim(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.released = append(s.released, id)

	return nil
}

func (s *fakeStore) releasedIDs() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]uuid.UUID(nil), s.released...)
}

func (s *fakeStore) appliedOutcomes() []store.ApplyCheckOutcomeParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]store.ApplyCheckOutcomeParams(nil), s.applied...)
}

func (s *fakeStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.claims)
}

// fakeChecker, kontrolün süresini ve sonucunu testin belirlemesini sağlıyor.
type fakeChecker struct {
	mu sync.Mutex

	delay  time.Duration
	err    error
	result checker.Result

	inFlight    int
	maxInFlight int
	runs        int
}

func (c *fakeChecker) Run(ctx context.Context, _ checker.Target) (checker.Result, error) {
	c.mu.Lock()
	c.runs++
	c.inFlight++
	if c.inFlight > c.maxInFlight {
		c.maxInFlight = c.inFlight
	}
	delay, err, result := c.delay, c.err, c.result
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}

	if err != nil {
		return checker.Result{}, err
	}

	if result.CheckedAt.IsZero() {
		result.CheckedAt = time.Now().UTC()
	}

	return result, nil
}

func (c *fakeChecker) stats() (runs, maxInFlight int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.runs, c.maxInFlight
}

// fakeRecorder, sonucun YAZILDIĞINI ve hangi tetikleyiciyle yazıldığını ölçer.
type fakeRecorder struct {
	mu sync.Mutex

	err      error
	recorded []recordedCall
}

type recordedCall struct {
	monitorID uuid.UUID
	trigger   store.CheckTrigger
	outcome   store.CheckOutcome
	ctxErr    error
}

func (r *fakeRecorder) Record(
	ctx context.Context, monitorID uuid.UUID, trigger store.CheckTrigger, result checker.Result,
) (store.CheckResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return store.CheckResult{}, r.err
	}

	r.recorded = append(r.recorded, recordedCall{
		monitorID: monitorID,
		trigger:   trigger,
		outcome:   result.Outcome,
		ctxErr:    ctx.Err(),
	})

	return store.CheckResult{}, nil
}

func (r *fakeRecorder) calls() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recordedCall(nil), r.recorded...)
}

// fakeIncidents, kesinti defterinin (4.1) sahtesi: hangi geçişlerin deftere
// ulaştığını kaydediyor. Gerçek defterin DAVRANIŞI burada değil, kendi
// paketinde ve gerçek veritabanına karşı test ediliyor.
type fakeIncidents struct {
	mu sync.Mutex

	err     error
	applied []appliedTransition
}

type appliedTransition struct {
	monitorID uuid.UUID
	from      store.MonitorStatus
	to        store.MonitorStatus
	changed   bool
	checkedAt time.Time
	ctxErr    error
}

func (f *fakeIncidents) ApplyTransition(
	ctx context.Context, monitor store.Monitor, transition monitors.Transition, checkedAt time.Time,
) (incidents.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return incidents.ActionNone, f.err
	}

	f.applied = append(f.applied, appliedTransition{
		monitorID: monitor.ID,
		from:      transition.From,
		to:        transition.Next.Status,
		changed:   transition.Changed,
		checkedAt: checkedAt,
		ctxErr:    ctx.Err(),
	})

	return incidents.ActionNone, nil
}

func (f *fakeIncidents) calls() []appliedTransition {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]appliedTransition(nil), f.applied...)
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testMonitors(count int) []store.Monitor {
	monitors := make([]store.Monitor, 0, count)
	for i := range count {
		monitors = append(monitors, store.Monitor{
			ID:                   uuid.New(),
			Name:                 fmt.Sprintf("monitor-%d", i),
			Url:                  fmt.Sprintf("https://example.com/%d", i),
			Method:               store.MonitorMethodGET,
			CheckIntervalSeconds: 60,
			TimeoutMs:            5000,
			ExpectedStatusMin:    200,
			ExpectedStatusMax:    299,
			FailureThreshold:     3,
			RecoveryThreshold:    2,
			IsActive:             true,
		})
	}

	return monitors
}

func successResult() checker.Result {
	return checker.Result{
		Outcome:      store.CheckOutcomeSuccess,
		HTTPStatus:   200,
		ResponseTime: 25 * time.Millisecond,
	}
}

// Sonuçlar ZAMANLANMIŞ olarak işaretlenmeli: 2.3'ün kararı gereği manuel
// kontroller uptime hesabına girmiyor, zamanlanmış olanlar giriyor. Tetikleyici
// yanlış yazılsaydı uptime yüzdesi sessizce boş kalırdı.
func TestRunDueRecordsScheduledResults(t *testing.T) {
	monitors := testMonitors(3)
	queries := &fakeStore{batches: [][]store.Monitor{monitors}}
	checks := &fakeChecker{result: successResult()}
	recorder := &fakeRecorder{}

	svc := NewService(queries, checks, recorder, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 5})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Claimed != 3 || stats.Recorded != 3 || stats.NotRun != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	calls := recorder.calls()
	if len(calls) != 3 {
		t.Fatalf("kaydedilen sonuç sayısı = %d, beklenen 3", len(calls))
	}

	for _, call := range calls {
		if call.trigger != store.CheckTriggerScheduled {
			t.Errorf("tetikleyici = %q, beklenen %q", call.trigger, store.CheckTriggerScheduled)
		}
	}

	if len(queries.appliedOutcomes()) != 3 {
		t.Errorf("defteri güncellenen monitor sayısı = %d, beklenen 3", len(queries.appliedOutcomes()))
	}
}

// logRecorder, tur özetinin GERÇEKTEN yazıldığını ölçmek için
// (internal/worker'daki `recorder` deseninin aynısı).
type logRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, record.Message)

	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) has(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, message := range r.messages {
		if strings.Contains(message, substr) {
			return true
		}
	}

	return false
}

// TUR ÖZETİ HER DURUMDA YAZILIYOR (7.5'in bulgusu).
//
// Döngü, parti DOLU geldiğinde devam ediyor; bir sonraki parti boş dönerse
// erken `return` ile çıkıyordu ve özet satırı hiç yazılmıyordu. Yani "12
// monitor kontrol edildi" turu, tam olarak birikinti eritilen durumda
// sessizce iz bırakmıyordu — kontroller çalıştı ama logda yok.
func TestRunDueLogsSummaryWhenBatchEndsExactlyFull(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{
		testMonitors(2), // dolu parti → döngü devam ediyor
		nil,             // boş parti → tur bitti
	}}

	logs := &logRecorder{}

	svc := NewService(queries, &fakeChecker{result: successResult()}, &fakeRecorder{},
		&fakeIncidents{}, &fakeBroadcaster{}, slog.New(logs), Options{MaxConcurrentChecks: 2})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Claimed != 2 {
		t.Fatalf("sahiplenilen = %d, beklenen 2", stats.Claimed)
	}

	if !logs.has("zamanlanmış kontroller tamamlandı") {
		t.Error("iş yapıldı ama tur özeti loglanmadı")
	}
}

// Boş turda özet YAZILMIYOR: 5 saniyede bir "0 kontrol" satırı gerçek
// olayları gürültüde boğardı (3.1'in `db-probe` kararının aynısı).
func TestRunDueStaysSilentWhenNothingIsDue(t *testing.T) {
	logs := &logRecorder{}

	svc := NewService(&fakeStore{}, &fakeChecker{result: successResult()}, &fakeRecorder{},
		&fakeIncidents{}, &fakeBroadcaster{}, slog.New(logs), Options{MaxConcurrentChecks: 2})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if logs.has("zamanlanmış kontroller tamamlandı") {
		t.Error("boş tur loglandı")
	}
}

// Eşzamanlılık SINIRLI: sınırsız bırakmak, aynı altyapıdaki hedefleri kendi
// ürettiğimiz yükle yavaşlatıp ölçümleri bozardı.
func TestRunDueLimitsConcurrency(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{testMonitors(12)}}
	checks := &fakeChecker{result: successResult(), delay: 40 * time.Millisecond}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 3})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	runs, maxInFlight := checks.stats()
	if runs != 12 {
		t.Errorf("kontrol sayısı = %d, beklenen 12", runs)
	}
	if maxInFlight > 3 {
		t.Errorf("aynı anda çalışan kontrol = %d, üst sınır 3", maxInFlight)
	}
}

// Sahiplenme partisi DOLDUYSA daha fazlası bekliyor demektir: birikinti tick
// başına bir parti kadar erimemeli (worker bir süre kapalı kaldıysa gecikme
// katlanarak büyürdü).
func TestRunDueDrainsBacklog(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{
		testMonitors(2), // dolu parti → devam
		testMonitors(2), // dolu parti → devam
		testMonitors(1), // eksik parti → bitti
	}}
	checks := &fakeChecker{result: successResult()}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 2})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Claimed != 5 {
		t.Errorf("sahiplenilen = %d, beklenen 5", stats.Claimed)
	}

	// Eksik parti geldiğinde dördüncü bir sorgu yapılmamalı.
	if queries.claimCount() != 3 {
		t.Errorf("claim çağrısı = %d, beklenen 3", queries.claimCount())
	}
}

// KONTROL HİÇ ÇALIŞMADIYSA (kapanma) sahte bir kesinti YAZILMIYOR ve sıra geri
// veriliyor — 2.2'nin ErrCheckNotRun ayrımının bu adımdaki karşılığı.
func TestRunDueReleasesClaimWhenCheckDidNotRun(t *testing.T) {
	monitors := testMonitors(1)
	queries := &fakeStore{batches: [][]store.Monitor{monitors}}
	checks := &fakeChecker{err: fmt.Errorf("%w: iptal", checker.ErrCheckNotRun)}
	recorder := &fakeRecorder{}

	svc := NewService(queries, checks, recorder, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.NotRun != 1 || stats.Recorded != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	if len(recorder.calls()) != 0 {
		t.Error("ölçüm alınmamışken sonuç yazıldı (sahte kesinti)")
	}

	released := queries.releasedIDs()
	if len(released) != 1 || released[0] != monitors[0].ID {
		t.Errorf("geri verilen sıra = %v, beklenen [%s]", released, monitors[0].ID)
	}
}

// ÖLÇÜM ALINDIKTAN SONRA kapanma sinyali gelse bile sonuç yazılıyor: gerçek
// bir ölçümü düşürmek, kesinti geçmişinde delik demek.
func TestRunDueWritesResultEvenWhenContextCancelled(t *testing.T) {
	monitors := testMonitors(1)
	queries := &fakeStore{batches: [][]store.Monitor{monitors}}
	checks := &fakeChecker{result: successResult()}
	recorder := &fakeRecorder{}

	svc := NewService(queries, checks, recorder, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	// Kontrol bitmiş, ardından kapanma sinyali gelmiş senaryosu: sahiplenilen
	// parti çalıştırılırken context iptal ediliyor.
	ctx, cancel := context.WithCancel(context.Background())
	checks.mu.Lock()
	checks.delay = 20 * time.Millisecond
	checks.mu.Unlock()

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	if _, err := svc.RunDue(ctx); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	calls := recorder.calls()
	if len(calls) != 1 {
		t.Fatalf("kaydedilen sonuç sayısı = %d, beklenen 1", len(calls))
	}

	// Yazma context'i iptal EDİLMEMİŞ olmalı (WithoutCancel).
	if calls[0].ctxErr != nil {
		t.Errorf("yazma context'i iptalli: %v", calls[0].ctxErr)
	}
}

// Kapanma sinyalinden sonra YENİ PARTİ sahiplenilmiyor.
func TestRunDueDoesNotClaimAfterCancel(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{testMonitors(2), testMonitors(2)}}
	checks := &fakeChecker{result: successResult()}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 2})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats, err := svc.RunDue(ctx)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Claimed != 0 || queries.claimCount() != 0 {
		t.Errorf("iptal sonrası sahiplenme yapıldı: stats=%+v, claim=%d", stats, queries.claimCount())
	}
}

// Sonuç YAZILAMAZSA bu gerçek bir veri kaybı: sayaçta ayrı duruyor (sessizce
// "başarılı" sayılmıyor).
func TestRunDueCountsRecordFailure(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{testMonitors(2)}}
	checks := &fakeChecker{result: successResult()}
	recorder := &fakeRecorder{err: errors.New("veritabanı yok")}

	svc := NewService(queries, checks, recorder, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 2})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Failed != 2 || stats.Recorded != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// Monitor satırındaki defter güncellenemezse sonuç GEÇERSİZ olmuyor: ölçüm
// zaten saklandı, eksik olan yalnızca durum/sayaç kaydı (bir sonraki kontrol
// düzeltiyor).
func TestRunDueKeepsResultWhenStateUpdateFails(t *testing.T) {
	queries := &fakeStore{
		batches:  [][]store.Monitor{testMonitors(1)},
		applyErr: errors.New("güncellenemedi"),
	}
	recorder := &fakeRecorder{}

	svc := NewService(queries, &fakeChecker{result: successResult()}, recorder,
		&fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Recorded != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	if len(recorder.calls()) != 1 {
		t.Error("sonuç yazılmadı")
	}
}

// Sahiplenme sorgusu hata verirse job hata döndürüyor (runtime bunu loglar);
// sessizce "iş yok" demek, kontrollerin durduğunu gizlemek olurdu.
func TestRunDueReturnsClaimError(t *testing.T) {
	queries := &fakeStore{claimErr: errors.New("bağlantı düştü")}

	svc := NewService(queries, &fakeChecker{}, &fakeRecorder{},
		&fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 2})

	if _, err := svc.RunDue(context.Background()); err == nil {
		t.Fatal("claim hatası yutuldu")
	}
}

// Parti boyu eşzamanlılık sınırıyla aynı olmalı: sahiplenip beklemek, o
// monitor'ün kontrolünü boşuna geciktirmek olurdu.
func TestRunDueClaimsAtMostConcurrencyLimit(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{testMonitors(1)}}

	svc := NewService(queries, &fakeChecker{result: successResult()}, &fakeRecorder{},
		&fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 7})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	queries.mu.Lock()
	defer queries.mu.Unlock()

	if len(queries.claims) == 0 || queries.claims[0] != 7 {
		t.Errorf("claim limiti = %v, beklenen 7", queries.claims)
	}
}

func TestNewServiceFallsBackToDefaultConcurrency(t *testing.T) {
	queries := &fakeStore{batches: [][]store.Monitor{testMonitors(1)}}

	svc := NewService(queries, &fakeChecker{result: successResult()}, &fakeRecorder{},
		&fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	queries.mu.Lock()
	defer queries.mu.Unlock()

	if queries.claims[0] != DefaultMaxConcurrentChecks {
		t.Errorf("claim limiti = %d, beklenen %d", queries.claims[0], DefaultMaxConcurrentChecks)
	}
}

// Job, runtime'a kaydedilebilir olmalı ve adı log'da görünür olmalı.
func TestJobIsRegisterable(t *testing.T) {
	svc := NewService(&fakeStore{}, &fakeChecker{}, &fakeRecorder{}, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{})

	job := svc.Job(5 * time.Second)
	if job.Name != JobName || job.Interval != 5*time.Second || job.Run == nil {
		t.Fatalf("job = %+v", job)
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("job koşusu hata verdi: %v", err)
	}
}

// DURUM MAKİNESİ SONUCU YAZILIYOR: scheduler kararı kendi vermiyor, saf
// fonksiyonun (monitors.NextState) çıktısını aynı yazmada defterliyor.
func TestRunDueAppliesStateMachineResult(t *testing.T) {
	// Eşiği 1 olan bir monitor: tek hata DOWN demek.
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 1
	monitor.RecoveryThreshold = 1

	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeTimeout,
	}}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	applied := queries.appliedOutcomes()
	if len(applied) != 1 {
		t.Fatalf("defter yazma sayısı = %d, beklenen 1", len(applied))
	}

	if applied[0].Status != store.MonitorStatusDown {
		t.Errorf("yazılan durum = %q, beklenen down", applied[0].Status)
	}
	if applied[0].ConsecutiveFailures != 1 || applied[0].ConsecutiveSuccesses != 0 {
		t.Errorf("sayaçlar = %d hata / %d başarı",
			applied[0].ConsecutiveFailures, applied[0].ConsecutiveSuccesses)
	}
	if applied[0].CheckedAt == nil {
		t.Error("son kontrol zamanı yazılmadı")
	}
}

// Eşik AŞILMADIKÇA durum değişmiyor — flapping önleme scheduler tarafında da
// görünür olmalı (yazılan durum eski durumun aynısı).
func TestRunDueKeepsStatusBelowThreshold(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 3
	monitor.RecoveryThreshold = 2

	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeConnectionRefused,
	}}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{}, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	applied := queries.appliedOutcomes()
	if len(applied) != 1 || applied[0].Status != store.MonitorStatusUp {
		t.Fatalf("yazılan durum = %+v, beklenen up (eşik aşılmadı)", applied)
	}
	if applied[0].ConsecutiveFailures != 1 {
		t.Errorf("ardışık hata = %d, beklenen 1", applied[0].ConsecutiveFailures)
	}
}

// 4.1'in kancası: durum değişimi kesinti defterine ULAŞIYOR ve deftere giden
// zaman KONTROLÜN zamanı (yazma anı değil — incident'ın başlangıcı ölçüme
// dayanmalı).
func TestRunDueForwardsTransitionToIncidentLedger(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 1

	checkedAt := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeTimeout,
		CheckedAt: checkedAt,
	}}
	ledger := &fakeIncidents{}

	svc := NewService(queries, checks, &fakeRecorder{}, ledger, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	applied := ledger.calls()
	if len(applied) != 1 {
		t.Fatalf("deftere ulaşan geçiş sayısı = %d, beklenen 1", len(applied))
	}

	if applied[0].from != store.MonitorStatusUp || applied[0].to != store.MonitorStatusDown {
		t.Errorf("geçiş = %q → %q, beklenen up → down", applied[0].from, applied[0].to)
	}
	if !applied[0].changed {
		t.Error("Changed=false geldi, beklenen true")
	}
	if !applied[0].checkedAt.Equal(checkedAt) {
		t.Errorf("deftere giden zaman = %v, beklenen %v", applied[0].checkedAt, checkedAt)
	}
}

// DURUM YAZILAMADIYSA DEFTERE DE DOKUNULMUYOR: monitor satırı hâlâ eski
// durumu gösteriyor, yani "değişti" diye bir olay gerçekleşmedi. Yazılsaydı
// bir sonraki kontrol aynı geçişi bir kez daha üretirdi.
func TestRunDueSkipsIncidentLedgerWhenStateUpdateFails(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 1

	queries := &fakeStore{
		batches:  [][]store.Monitor{{monitor}},
		applyErr: errors.New("güncellenemedi"),
	}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeTimeout,
	}}
	ledger := &fakeIncidents{}

	svc := NewService(queries, checks, &fakeRecorder{}, ledger, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if calls := ledger.calls(); len(calls) != 0 {
		t.Fatalf("defter çağrıldı (%+v), beklenen: çağrılmaması", calls)
	}
}

// Defterin hatası ÖLÇÜMÜ DÜŞÜRMÜYOR: kontrol çalıştı, sonuç yazıldı; eksik
// olan kesinti kaydı (loglanıyor, bkz. applyIncident).
func TestRunDueKeepsResultWhenIncidentLedgerFails(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 1

	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeTimeout,
	}}
	recorder := &fakeRecorder{}

	svc := NewService(queries, checks, recorder, &fakeIncidents{err: errors.New("defter yazılamadı")},
		&fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	stats, err := svc.RunDue(context.Background())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Recorded != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, beklenen 1 kayıt / 0 hata", stats)
	}
	if len(recorder.calls()) != 1 {
		t.Error("sonuç yazılmadı")
	}
}

// Durum DEĞİŞMEDİYSE defter yine de çağrılıyor (kararı defter veriyor:
// Changed=false → ActionNone). Kancanın "yalnızca değişimde çağır" filtresi
// scheduler'da değil, defterin kendisinde — kural tek bir yerde yazılı.
func TestRunDueCallsLedgerWithUnchangedTransition(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp

	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	ledger := &fakeIncidents{}

	svc := NewService(queries, &fakeChecker{result: successResult()}, &fakeRecorder{},
		ledger, &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	calls := ledger.calls()
	if len(calls) != 1 {
		t.Fatalf("defter çağrı sayısı = %d, beklenen 1", len(calls))
	}
	if calls[0].changed {
		t.Error("Changed=true geldi, beklenen false (up → up)")
	}
}

// fakeBroadcaster, canlı yayın duyurularını kaydeder (6.2).
type fakeBroadcaster struct {
	mu    sync.Mutex
	calls []broadcastCall
}

type broadcastCall struct {
	orgID    uuid.UUID
	monitors []uuid.UUID
	reason   string
}

func (f *fakeBroadcaster) Changed(_ context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, broadcastCall{orgID: orgID, monitors: monitorIDs, reason: reason})
}

func (f *fakeBroadcaster) snapshot() []broadcastCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]broadcastCall(nil), f.calls...)
}

// --- Canlı yayın (6.2) -------------------------------------------------------

// Duyuru DURUM DEĞİŞİMİNİN GEÇTİĞİ TEK NOKTADAN çıkıyor: kancanın burada
// olmasının sebebi, "bu monitor down mu" sorusuna ikinci bir cevap yazılırsa
// canlı yayının panodan ayrışacak olması.
func TestRunDueBroadcastsOnStatusChange(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 1

	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeTimeout,
	}}
	broadcaster := &fakeBroadcaster{}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{},
		broadcaster, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	calls := broadcaster.snapshot()
	if len(calls) != 1 {
		t.Fatalf("duyuru sayısı = %d, beklenen 1", len(calls))
	}

	if calls[0].orgID != monitor.OrgID {
		t.Errorf("org = %s, beklenen %s", calls[0].orgID, monitor.OrgID)
	}

	// PUBLIC KANALIN KAPISINA verilecek liste tam olarak değişen monitor.
	if len(calls[0].monitors) != 1 || calls[0].monitors[0] != monitor.ID {
		t.Errorf("monitor listesi = %v, beklenen [%s]", calls[0].monitors, monitor.ID)
	}
}

// DEĞİŞMEYEN KONTROLDE DUYURU YOK: 100 monitor × dakikada bir kontrol, her
// turda sinyal göndermek demek olurdu — istemci sürekli tazelerdi.
func TestRunDueDoesNotBroadcastWhenNothingChanged(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp

	queries := &fakeStore{batches: [][]store.Monitor{{monitor}}}
	broadcaster := &fakeBroadcaster{}

	svc := NewService(queries, &fakeChecker{result: successResult()}, &fakeRecorder{},
		&fakeIncidents{}, broadcaster, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if calls := broadcaster.snapshot(); len(calls) != 0 {
		t.Fatalf("değişmeyen kontrolde duyuru yapıldı: %+v", calls)
	}
}

// DURUM YAZILAMADIYSA DUYURU DA YOK: defterle aynı gerekçe — monitor satırı
// hâlâ eski durumu gösteriyor, yani "değişti" diye bir olay gerçekleşmedi.
func TestRunDueDoesNotBroadcastWhenStateUpdateFails(t *testing.T) {
	monitor := testMonitors(1)[0]
	monitor.CurrentStatus = store.MonitorStatusUp
	monitor.FailureThreshold = 1

	queries := &fakeStore{
		batches:  [][]store.Monitor{{monitor}},
		applyErr: errors.New("güncellenemedi"),
	}
	checks := &fakeChecker{result: checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		ErrorType: store.CheckErrorTypeTimeout,
	}}
	broadcaster := &fakeBroadcaster{}

	svc := NewService(queries, checks, &fakeRecorder{}, &fakeIncidents{},
		broadcaster, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(context.Background()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if calls := broadcaster.snapshot(); len(calls) != 0 {
		t.Fatalf("durum yazılamadığı hâlde duyuru yapıldı: %+v", calls)
	}
}
