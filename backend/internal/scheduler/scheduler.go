// Package scheduler, zamanlanmış kontrolleri yürütür: sırası gelen
// monitor'leri veritabanından ATOMİK olarak sahiplenir, kontrolleri sınırlı
// eşzamanlılıkla çalıştırır ve sonuçları zaman serisine yazar.
//
// Sorumluluk sınırları (bu projede paketler bilinçli olarak dar):
//
//	internal/checker   → tek bir kontrolü ÇALIŞTIRIR (2.2)
//	internal/results   → sonucu SAKLAR (2.3)
//	internal/worker    → süreç çatısı: periyodik job, drain'li kapanma (3.1)
//	internal/monitors  → NextState: sonuç durumu nasıl değiştirir, SAF (3.3)
//	internal/incidents → durum değişiminin kesinti defterine yansıması (4.1)
//	internal/scheduler → SIRASI GELENİ BULUR ve yukarıdakileri birbirine bağlar
//
// Durum KARARI burada değil (3.3): bu paket saf `monitors.NextState`
// fonksiyonunu çağırıp çıktısını yazıyor. Tek bir başarısız kontrolden "DOWN"
// sonucuna atlamak flapping'in kendisi olurdu. Aynı sebeple 4.1'in incident
// açma/kapama kancası da tam olarak buraya takılı: durum değişimi TEK BİR
// yerden geçtiği için kesinti kaydı da tek bir yerden doğuyor.
//
// Kuyruk kütüphanesi YOK (0.2 kararı): eşzamanlılık garantileri Postgres'in
// `FOR UPDATE SKIP LOCKED` mekanizmasıyla kuruluyor (bkz. db/queries/schedule.sql).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/worker"
)

// JobName, runtime'da görünen job adı.
const JobName = "scheduler"

// DefaultMaxConcurrentChecks, aynı anda çalışabilecek kontrol sayısı.
//
// Sınır neden var: kontroller ağ bekleyen işler, yani "hepsini birden başlat"
// teknik olarak mümkün. Ama 500 monitor'ün kontrolünü aynı anda açmak hem bu
// makinenin dosya tanıtıcılarını/soketlerini hem de aynı altyapıda duran hedef
// sistemleri (çoğu monitor aynı birkaç sunucuyu gösteriyor) zorlar; ölçümler de
// kendi ürettiğimiz yük yüzünden yavaşlar — yani ölçüm aracı ölçtüğü şeyi
// bozar.
const DefaultMaxConcurrentChecks = 10

const (
	// dbWriteTimeout, kontrol bittikten sonraki yazmaların üst sınırı
	// (normal çalışma).
	dbWriteTimeout = 5 * time.Second

	// shutdownWriteTimeout, KAPANMA sırasında (context zaten iptalliyken)
	// yapılan yazmaların üst sınırı.
	//
	// Kısa tutulması zorunlu: runtime, job'ları iptal ettikten sonra onlara
	// yalnızca birkaç saniye tanıyor (bkz. internal/worker → cancelSlack).
	// Daha uzun bir sınır, sürecin "kapanmayan job" hatasıyla çıkmasına yol
	// açardı.
	shutdownWriteTimeout = time.Second
)

// Store, scheduler'ın veritabanından ihtiyaç duyduğu sorgular.
type Store interface {
	ClaimDueMonitors(ctx context.Context, rowLimit int32) ([]store.Monitor, error)
	ApplyCheckOutcome(ctx context.Context, arg store.ApplyCheckOutcomeParams) error
	ReleaseMonitorClaim(ctx context.Context, id uuid.UUID) error
}

// Checker, tek bir kontrolü çalıştıran bağımlılık (internal/checker).
type Checker interface {
	Run(ctx context.Context, target checker.Target) (checker.Result, error)
}

// Recorder, sonucu saklayan bağımlılık (internal/results).
type Recorder interface {
	Record(
		ctx context.Context, monitorID uuid.UUID, trigger store.CheckTrigger, result checker.Result,
	) (store.CheckResult, error)
}

// IncidentLedger, durum değişimini kesinti defterine yansıtan bağımlılık
// (internal/incidents, 4.1).
//
// ZORUNLU bir bağımlılık, opsiyonel değil: nil geçilebilseydi worker'ın
// kurulumunda unutulan tek bir satır, kontrollerin çalışmaya devam ettiği ama
// hiçbir kesintinin kaydedilmediği sessiz bir üretim hatası olurdu.
type IncidentLedger interface {
	ApplyTransition(
		ctx context.Context, monitor store.Monitor, transition monitors.Transition, checkedAt time.Time,
	) (incidents.Action, error)
}

// Broadcaster, durum değişimini canlı yayına duyuran bağımlılık
// (internal/realtime, 6.2).
//
// KANCA BURADA, ÇÜNKÜ DURUM DEĞİŞİMİ BURADAN GEÇİYOR. "Bu monitor down mu"
// sorusunun ikinci bir cevabı yazılsaydı, canlı yayının söylediğiyle panonun
// okuduğu ayrışırdı — 4.1'in `ApplyTransition`ı da tam bu gerekçeyle aynı
// noktaya takılmıştı.
//
// `Changed` hata DÖNDÜRMÜYOR: yayın, kontrolün ya da kesinti kaydının
// başarısının parçası değil.
type Broadcaster interface {
	Changed(ctx context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID, reason string)
}

// Options, scheduler ayarları.
type Options struct {
	// MaxConcurrentChecks, eşzamanlı kontrol sayısı üst sınırı. 0 ise
	// DefaultMaxConcurrentChecks.
	MaxConcurrentChecks int
}

// Service, zamanlanmış kontrolleri yürütür.
type Service struct {
	queries       Store
	checks        Checker
	results       Recorder
	incidents     IncidentLedger
	broadcaster   Broadcaster
	logger        *slog.Logger
	maxConcurrent int
}

// NewService, servisi kurar.
//
// `broadcaster` ZORUNLU (IncidentLedger ile aynı gerekçe): nil geçilebilseydi,
// worker kurulumunda unutulan tek bir satır "kontroller çalışıyor ama arayüz
// hiç canlanmıyor" biçiminde, ancak elle fark edilen bir hata olurdu.
func NewService(
	queries Store, checks Checker, results Recorder, ledger IncidentLedger,
	broadcaster Broadcaster, logger *slog.Logger, opts Options,
) *Service {
	maxConcurrent := opts.MaxConcurrentChecks
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentChecks
	}

	return &Service{
		queries:       queries,
		checks:        checks,
		results:       results,
		incidents:     ledger,
		broadcaster:   broadcaster,
		logger:        logger,
		maxConcurrent: maxConcurrent,
	}
}

// Stats, bir turun sonucu (log ve testler için).
type Stats struct {
	// Claimed, sahiplenilen monitor sayısı.
	Claimed int
	// Recorded, ölçümü alınıp zaman serisine yazılan kontrol sayısı.
	Recorded int
	// NotRun, hiç çalışamayan kontrol sayısı (kapanma) — sırası geri verildi.
	NotRun int
	// Failed, ölçüm alınmasına rağmen YAZILAMAYAN kontrol sayısı: gerçek veri
	// kaybı, sessiz kalmaması gereken tek durum.
	Failed int
}

func (s *Stats) add(other Stats) {
	s.Claimed += other.Claimed
	s.Recorded += other.Recorded
	s.NotRun += other.NotRun
	s.Failed += other.Failed
}

// Job, scheduler'ı worker runtime'ına kaydedilebilir bir job'a çevirir.
func (s *Service) Job(interval time.Duration) worker.Job {
	return worker.Job{
		Name:     JobName,
		Interval: interval,
		Run: func(ctx context.Context) error {
			_, err := s.RunDue(ctx)

			return err
		},
	}
}

// RunDue, sırası gelmiş monitor'leri kontrol eder.
//
// Tek turda bitmesi ŞART DEĞİL: sahiplenme partiler hâlinde yapılıyor ve bir
// parti tamamen dolduysa (demek ki daha fazlası bekliyor) döngü devam ediyor.
// Aksi hâlde gecikmiş bir birikinti (worker bir süre kapalı kaldı) tick
// başına yalnızca `maxConcurrent` kadar erirdi.
//
// Kapanma sinyali geldiğinde YENİ PARTİ SAHİPLENİLMİYOR: devam eden kontroller
// bitiyor (runtime'ın drain'i, 3.1) ama sıraya yeni iş alınmıyor.
func (s *Service) RunDue(ctx context.Context) (Stats, error) {
	var total Stats

	// Çıkış yolu TEK: döngü nasıl biterse bitsin özet aşağıdan geçiyor.
	// Önceki hâlde "parti dolu → devam et → sonraki parti boş" dizisinde
	// erken bir `return` vardı ve tam da birikinti eritilen tur sessizce iz
	// bırakmıyordu: kontroller çalışıyor ama logda görünmüyordu.
	defer func() {
		if total.Claimed > 0 {
			s.logger.Info("zamanlanmış kontroller tamamlandı",
				slog.Int("claimed", total.Claimed),
				slog.Int("recorded", total.Recorded),
				slog.Int("not_run", total.NotRun),
				slog.Int("failed", total.Failed))
		}
	}()

	for {
		if ctx.Err() != nil {
			return total, nil
		}

		//nolint:gosec // maxConcurrent yapılandırmadan doğrulanarak geliyor
		due, err := s.queries.ClaimDueMonitors(ctx, int32(s.maxConcurrent))
		if err != nil {
			// Kapanma sırasında iptal edilen sorgu bir arıza değil.
			if ctx.Err() != nil {
				return total, nil
			}

			return total, fmt.Errorf("sırası gelen monitor'ler sahiplenilemedi: %w", err)
		}

		if len(due) == 0 {
			return total, nil
		}

		total.add(s.runBatch(ctx, due))

		// Parti dolmadıysa bekleyen başka monitor yok.
		if len(due) < s.maxConcurrent {
			return total, nil
		}
	}
}

// runBatch, sahiplenilmiş monitor'lerin kontrollerini SINIRLI eşzamanlılıkla
// çalıştırır ve hepsi bitene kadar bekler.
//
// Beklemek önemli: job dönmeden runtime kapanmayı tamamlamıyor (3.1'in
// drain'i), yani "devam eden kontroller bitsin" garantisi buradaki
// `wg.Wait()`e dayanıyor.
func (s *Service) runBatch(ctx context.Context, batch []store.Monitor) Stats {
	var (
		mu    sync.Mutex
		stats = Stats{Claimed: len(batch)}
		wg    sync.WaitGroup
	)

	// Goroutine havuzu: semafor kanalı. Parti boyu bugün eşzamanlılık sınırıyla
	// aynı (sahiplenilen iş hemen başlayabilsin — sahiplenip beklemek, o
	// monitor'ün kontrolünü boşuna geciktirmek olurdu), ama sınır burada
	// AYRICA duruyor: parti boyu ileride büyürse eşzamanlılık garantisi
	// kendiliğinden bozulmasın.
	slots := make(chan struct{}, s.maxConcurrent)

	for _, monitor := range batch {
		wg.Add(1)
		slots <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			result := s.checkOne(ctx, monitor)

			mu.Lock()
			defer mu.Unlock()

			switch result {
			case outcomeRecorded:
				stats.Recorded++
			case outcomeNotRun:
				stats.NotRun++
			case outcomeFailed:
				stats.Failed++
			}
		}()
	}

	wg.Wait()

	return stats
}

// checkOutcome, tek bir monitor turunun sonucu (istatistik için).
type checkOutcome int

const (
	outcomeRecorded checkOutcome = iota
	outcomeNotRun
	outcomeFailed
)

// checkOne, tek bir monitor'ü kontrol eder ve sonucu yazar.
func (s *Service) checkOne(ctx context.Context, monitor store.Monitor) checkOutcome {
	result, err := s.checks.Run(ctx, checker.TargetOf(monitor))
	if err != nil {
		// KONTROL HİÇ ÇALIŞMADI (2.2'nin ErrCheckNotRun ayrımı): elimizde ölçüm
		// yok. Başarısız kontrol olarak yazmak, worker'ın her kapanmasının
		// sahte bir kesinti kaydı (ve 4.1'de gerçek olmayan bir incident)
		// üretmesi olurdu. Sıra geri veriliyor ki monitor bir tur atlamasın.
		if errors.Is(err, checker.ErrCheckNotRun) {
			s.releaseClaim(ctx, monitor)

			return outcomeNotRun
		}

		// checker sözleşmesine göre buraya düşülmemeli (hedefin cevap
		// vermemesi bir SONUÇ, hata değil); düşülüyorsa checker'da bug var.
		s.logger.Error("kontrol beklenmeyen bir hatayla döndü",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("error", err.Error()))

		return outcomeFailed
	}

	// ÖLÇÜM ALINDI: buradan sonrası kapanma sinyaline rağmen tamamlanıyor
	// (bkz. writeContext) — gerçek bir ölçümü çöpe atmamak için.
	writeCtx, cancel := s.writeContext(ctx)
	defer cancel()

	if _, err := s.results.Record(writeCtx, monitor.ID, store.CheckTriggerScheduled, result); err != nil {
		// Tek gerçek veri kaybı senaryosu: ölçüm var, satır yok.
		s.logger.Error("zamanlanmış kontrol sonucu kaydedilemedi",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("outcome", string(result.Outcome)),
			slog.String("error", err.Error()))

		return outcomeFailed
	}

	// DURUM MAKİNESİ (3.3): karar tek bir kontrole değil ARDIŞIK sonuçlara
	// bakıyor ve saf bir fonksiyonda duruyor (monitors.NextState). Buradaki iş
	// yalnızca girdiyi vermek ve çıktıyı yazmak.
	transition := monitors.NextState(monitors.StateOf(monitor), result.Outcome)

	if err := s.queries.ApplyCheckOutcome(writeCtx, store.ApplyCheckOutcomeParams{
		ID:                   monitor.ID,
		CheckedAt:            &result.CheckedAt,
		Status:               transition.Next.Status,
		ConsecutiveFailures:  transition.Next.ConsecutiveFailures,
		ConsecutiveSuccesses: transition.Next.ConsecutiveSuccesses,
	}); err != nil {
		// Ölçüm zaten yazıldı; eksik olan monitor satırındaki defter kaydı.
		// Sonucu "başarısız" saymıyoruz — bir sonraki kontrol sayaçları
		// yeniden hesaplayıp düzeltiyor, ama sessiz de kalmıyor.
		s.logger.Warn("monitor durumu güncellenemedi",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("error", err.Error()))

		// INCIDENT DEFTERİNE DOKUNULMUYOR: monitor satırı hâlâ eski durumu
		// gösteriyor, yani "değişti" diye bir olay henüz gerçekleşmedi.
		// Yazılsaydı, sonraki kontrol aynı geçişi bir kez daha üretirdi.
		s.logResult(monitor, result, transition)

		return outcomeRecorded
	}

	// KESİNTİ DEFTERİ (4.1): durum değişimi tek bir yerden geçiyor, dolayısıyla
	// incident açma/kapama da tam olarak burada. İkinci bir yerde "bu monitor
	// down mu" hesaplamak, monitor'ün durumuyla çelişen incident'lar demekti.
	action := s.applyIncident(writeCtx, monitor, transition, result.CheckedAt)

	// CANLI YAYIN (6.2): duyuru defter yazıldıktan SONRA, yani olayı alan
	// istemci tazelediğinde yeni durumu görmesi garanti. Yalnızca gerçekten
	// değişen bir şey varsa yayın yapılıyor — her başarılı kontrolde sinyal
	// göndermek, 100 monitor × dakikada bir kontrol = saniyede iki tazeleme
	// demekti.
	if transition.Changed || action == incidents.ActionOpened || action == incidents.ActionResolved {
		s.broadcaster.Changed(writeCtx, monitor.OrgID, []uuid.UUID{monitor.ID}, broadcastReason(action))
	}

	s.logResult(monitor, result, transition)

	return outcomeRecorded
}

// broadcastReason, yayın sebebini adlandırır (yalnızca tanı için — istemci
// sebebe bakarak farklı davranmıyor, bkz. internal/realtime/broadcast.go).
func broadcastReason(action incidents.Action) string {
	switch action {
	case incidents.ActionOpened:
		return realtime.ReasonIncidentOpened
	case incidents.ActionResolved:
		return realtime.ReasonIncidentClosed
	default:
		return realtime.ReasonMonitorStatus
	}
}

// applyIncident, durum değişimini kesinti defterine yansıtır (4.1).
//
// Hata İSTEĞİ BAŞARISIZ SAYMIYOR ama `Error` ile loglanıyor: ölçüm ve monitor
// durumu doğru yazıldı, eksik olan kesinti kaydı. Kendini toparlamıyor —
// bir sonraki kontrol aynı geçişi üretmeyecek (monitor artık zaten `down`),
// yani o kesinti incident'sız kalır. Alternatifi (her başarısız kontrolde
// defteri yeniden mutabık kılmak) her `down` monitor'ün her turuna fazladan
// bir yazma sorgusu eklerdi; nadir bir yazma hatasının bedeli, sıcak yolun
// sürekli maliyetinden düşük.
func (s *Service) applyIncident(
	ctx context.Context, monitor store.Monitor, transition monitors.Transition, checkedAt time.Time,
) incidents.Action {
	action, err := s.incidents.ApplyTransition(ctx, monitor, transition, checkedAt)
	if err != nil {
		s.logger.Error("kesinti defteri güncellenemedi",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("from", string(transition.From)),
			slog.String("to", string(transition.Next.Status)),
			slog.String("error", err.Error()))

		return incidents.ActionNone
	}

	if action != incidents.ActionNone {
		s.logger.Debug("kesinti defteri güncellendi",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("action", string(action)))
	}

	return action
}

// releaseClaim, çalıştırılamamış bir kontrolün sırasını geri verir.
func (s *Service) releaseClaim(ctx context.Context, monitor store.Monitor) {
	// Buraya kapanma sırasında geliniyor, yani ctx büyük olasılıkla İPTALLİ:
	// yazma ayrı bir context'e ihtiyaç duyuyor, yoksa sıra geri verilemez ve
	// monitor bir tur (aralığı kadar) körleşirdi.
	releaseCtx, cancel := s.writeContext(ctx)
	defer cancel()

	if err := s.queries.ReleaseMonitorClaim(releaseCtx, monitor.ID); err != nil {
		s.logger.Warn("kontrol çalıştırılamadı ve sıra geri verilemedi",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("error", err.Error()))

		return
	}

	s.logger.Info("kontrol çalıştırılamadı, sıra geri verildi",
		slog.String("monitor_id", monitor.ID.String()))
}

// writeContext, ölçüm SONRASI yazmalar için context üretir.
//
// İki rejim var ve ikisi de bilinçli:
//
//   - Normal çalışma: çağıranın context'inden türetiliyor (iptal yayılıyor),
//     üst sınır dbWriteTimeout. Veritabanı yavaşsa yazma 5 saniyeye kadar
//     deneniyor — ölçümü kaybetmemek için.
//   - Kapanma (ctx zaten iptalli): iptal KOPARILIYOR
//     (`context.WithoutCancel`), üst sınır shutdownWriteTimeout. Sebep:
//     kontrol çalıştı, sonuç elimizde; kapanma sinyali yüzünden onu düşürmek
//     gerçek bir ölçümü çöpe atmak olurdu. Sınır kısa çünkü runtime iptalden
//     sonra job'lara yalnızca birkaç saniye tanıyor.
func (s *Service) writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithTimeout(ctx, dbWriteTimeout)
	}

	return context.WithTimeout(context.WithoutCancel(ctx), shutdownWriteTimeout)
}

// logResult, tek bir kontrolün sonucunu loglar.
//
// Başarılı kontroller `Debug`: 100 monitor × dakikada bir kontrol = günde 144
// bin satır log, hepsi "her şey yolunda" diyor. Yavaş ve başarısız kontroller
// `Info`/`Warn`, çünkü onlar gerçekten olay. Durum DEĞİŞİMİNİ loglamak 3.3'ün
// işi (bu paket tek tek kontrollere bakıyor, geçmişe değil).
func (s *Service) logResult(monitor store.Monitor, result checker.Result, transition monitors.Transition) {
	attrs := []any{
		slog.String("monitor_id", monitor.ID.String()),
		slog.String("url", monitor.Url),
		slog.String("outcome", string(result.Outcome)),
		slog.Int64("response_ms", result.ResponseTime.Milliseconds()),
	}

	// DURUM DEĞİŞİMİ tek tek kontrollerden daha önemli bir olay: eşik aşıldı,
	// yani kullanıcıya "servis düştü/geri geldi" denecek an bu (4.1'in
	// incident'ı da buradan doğacak). Bu yüzden ayrı ve her zaman görünür.
	if transition.Changed {
		s.logger.Info("monitor durumu değişti",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("name", monitor.Name),
			slog.String("from", string(transition.From)),
			slog.String("to", string(transition.Next.Status)),
			slog.Int("consecutive_failures", int(transition.Next.ConsecutiveFailures)),
			slog.Int("consecutive_successes", int(transition.Next.ConsecutiveSuccesses)))
	}

	switch result.Outcome {
	case store.CheckOutcomeSuccess:
		s.logger.Debug("kontrol başarılı", attrs...)
	case store.CheckOutcomeDegraded:
		s.logger.Info("kontrol yavaş", attrs...)
	case store.CheckOutcomeFailure:
		s.logger.Warn("kontrol başarısız",
			append(attrs, slog.String("error_type", string(result.ErrorType)))...)
	}
}
