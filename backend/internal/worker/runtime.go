// Package worker, arka plan sürecinin İŞ ÇATISIDIR: periyodik job'ları
// çalıştırır, hatalarını/paniklerini yalıtır ve kapanma sinyalinde devam eden
// işi düzgün sonlandırır.
//
// Kapsam sınırı bilinçli: bu paket ne bir kontrolün nasıl yapıldığını bilir
// (internal/checker) ne sonucun nasıl saklandığını (internal/results) ne de
// hangi monitor'ün sırası geldiğini (3.2'nin scheduler'ı). Bildiği tek şey
// "şu işi şu aralıkta çalıştır, düşerse ayakta kal, kapanırken toparla".
// 3.2/3.4'ün job'ları bu çatıya birer `Job` olarak takılacak; süreç
// yönetimini her job'ın yeniden yazması gerekmiyor.
//
// api süreciyle karşılaştırma: orada `http.Server` bu rolü oynuyor (istekleri
// dağıtır, panik kurtarır, Shutdown ile drain eder). worker'ın HTTP sunucusu
// olmadığı için aynı işi bu paket yapıyor.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// ErrNoJobs, hiç job kaydedilmeden çalıştırılan bir runtime'ı bildirir.
//
// Sessizce boş dönmek istenmiyor: iş yapmayan ama "çalışıyor" görünen bir
// worker, monitor'lerin kontrol edilmediğini kimsenin fark etmediği tam
// olarak o durum.
var ErrNoJobs = errors.New("worker: kayıtlı job yok")

// ErrShutdownIncomplete, job'ların iptal edildikten sonra da dönmediğini
// bildirir. Süreç bu durumda hatayla çıkıyor (exit 1): "düzgün kapandım"
// demek, kapanmayan bir job'ı gizlemek olurdu.
var ErrShutdownIncomplete = errors.New("worker: job'lar iptal sonrası durmadı")

// heartbeatJobName, runtime'ın kendi kalp atışı job'ının adı. Kayıtlı
// job'larla çakışmaması için rezerve.
const heartbeatJobName = "heartbeat"

// cancelSlack, iptal edildikten SONRA job'lara verilen son süre.
//
// Sınırsız beklenemez: `context`ini yok sayan bir job süreci sonsuza kadar
// kapanmaz hâle getirirdi (orkestratör sonunda SIGKILL gönderir, o da bu
// hatanın loglanmadığı bir kapanma demek).
const cancelSlack = 2 * time.Second

// Job, runtime'ın periyodik olarak çalıştırdığı bir iş.
type Job struct {
	// Name log satırlarında ve hata mesajlarında görünür; benzersiz olmalı.
	Name string
	// Interval, iki koşunun BAŞLANGIÇLARI arasındaki en az süre.
	Interval time.Duration
	// Run, işin kendisi. Aldığı context kapanma sırasında iptal EDİLMEZ —
	// önce drain süresi tanınır (bkz. Runtime.Run).
	Run func(ctx context.Context) error
}

// Heartbeat, sürecin canlılık sinyali ayarları.
//
// worker'ın HTTP portu yok, yani api'nin `/health` ucu gibi yoklanamıyor.
// Onun yerine runtime periyodik olarak bir dosyaya zaman damgası yazıyor;
// `worker healthcheck` alt komutu (ve compose'daki healthcheck) o dosyanın
// TAZELİĞİNE bakıyor. Böylece donmuş bir worker konteyner düzeyinde
// "running" görünmeye devam etmiyor.
type Heartbeat struct {
	// Path boşsa kalp atışı kapalı (testler ve dosya sistemine yazamayan
	// dağıtımlar için).
	Path   string
	Period time.Duration
}

// Options, runtime'ın kurulum ayarları.
type Options struct {
	Logger *slog.Logger
	// ShutdownGrace, kapanma sinyalinden sonra DEVAM EDEN job'lara tanınan
	// süre. Bitmeden dolarsa job'ların context'i iptal edilir.
	//
	// Neden hemen iptal edilmiyor: 3.2'de devam eden bir kontrol yarıda
	// kesilirse `checker.Run` onu ErrCheckNotRun ile bildirir, yani ölçüm
	// kaybedilir. Kapanma sinyali alan bir worker'ın elindeki kontrolü
	// bitirmesi, sonra çıkması gerekiyor.
	ShutdownGrace time.Duration
	Heartbeat     Heartbeat
}

// Runtime, kayıtlı job'ları çalıştıran süreç çatısı.
type Runtime struct {
	logger    *slog.Logger
	grace     time.Duration
	heartbeat Heartbeat

	jobs  []Job
	names map[string]struct{}

	// now, testlerin saati sabitleyebilmesi için (üretimde time.Now).
	now func() time.Time
}

// New, runtime'ı kurar.
func New(opts Options) *Runtime {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	grace := opts.ShutdownGrace
	if grace < 0 {
		grace = 0
	}

	return &Runtime{
		logger:    logger,
		grace:     grace,
		heartbeat: opts.Heartbeat,
		names:     make(map[string]struct{}),
		now:       time.Now,
	}
}

// Register, bir job'ı kaydeder.
//
// Doğrulama BAŞLARKEN yapılıyor: aralığı sıfır olan bir job'ın çalışma anında
// `time.NewTicker` panikletmesi yerine, süreç ilk saniyesinde anlaşılır bir
// hatayla ölüyor (config paketinin aynı disiplini).
func (r *Runtime) Register(job Job) error {
	switch {
	case job.Name == "":
		return errors.New("worker: job adı boş olamaz")
	case job.Name == heartbeatJobName:
		return fmt.Errorf("worker: %q adı runtime'ın kalp atışı için rezerve", heartbeatJobName)
	case job.Interval <= 0:
		return fmt.Errorf("worker: %q job'unun aralığı pozitif olmalı: %s", job.Name, job.Interval)
	case job.Run == nil:
		return fmt.Errorf("worker: %q job'unun Run alanı boş", job.Name)
	}

	if _, exists := r.names[job.Name]; exists {
		return fmt.Errorf("worker: %q adında bir job zaten kayıtlı", job.Name)
	}

	r.names[job.Name] = struct{}{}
	r.jobs = append(r.jobs, job)

	return nil
}

// JobNames, kayıtlı job adlarını (kalp atışı dahil) döner — açılış logu için.
func (r *Runtime) JobNames() []string {
	names := make([]string, 0, len(r.jobs)+1)
	for _, job := range r.jobs {
		names = append(names, job.Name)
	}

	if r.heartbeat.Path != "" {
		names = append(names, heartbeatJobName)
	}

	return names
}

// Run, job'ları çalıştırır ve ctx iptal edilene kadar bloklar.
//
// KAPANMA SIRASI:
//  1. ctx iptal edilir (SIGINT/SIGTERM) → yeni koşu BAŞLATILMAZ.
//  2. Devam eden koşulara ShutdownGrace kadar süre tanınır; bu süre boyunca
//     job'ların context'i HÂLÂ canlıdır (elindeki işi bitirebilir).
//  3. Süre dolarsa job context'i iptal edilir; cancelSlack içinde de
//     dönmeyen job varsa ErrShutdownIncomplete döner.
func (r *Runtime) Run(ctx context.Context) error {
	if len(r.jobs) == 0 {
		return ErrNoJobs
	}

	// Kopya: kalp atışı job'ı r.jobs'a EKLENMİYOR (Run ikinci kez çağrılsa
	// listede iki kalp atışı olurdu).
	jobs := make([]Job, len(r.jobs), len(r.jobs)+1)
	copy(jobs, r.jobs)

	if r.heartbeat.Path != "" && r.heartbeat.Period > 0 {
		// Kalp atışı da sıradan bir job: aynı döngü makinesinden geçmesi,
		// dosyanın tazeliğinin "runtime gerçekten tur atıyor" demesini
		// sağlıyor. Ayrı bir goroutine olsaydı, job döngüleri tıkanmışken de
		// tazelenmeye devam ederdi.
		jobs = append(jobs, Job{
			Name:     heartbeatJobName,
			Interval: r.heartbeat.Period,
			Run: func(context.Context) error {
				return WriteHeartbeat(r.heartbeat.Path, r.now())
			},
		})
	}

	// Job'ların context'i sinyalden BAĞIMSIZ: `WithoutCancel` ile ctx'in
	// iptali buraya yayılmıyor, iptal kararını aşağıdaki drain mantığı
	// veriyor. (Değerler korunuyor, iptal kopuyor.)
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)

		go func() {
			defer wg.Done()
			r.loop(ctx, jobCtx, job)
		}()
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
	case <-done:
		// Job döngüleri kendiliğinden bitmez; buraya düşmek yalnızca ctx
		// zaten iptalliyken Run çağrıldığında olur.
		return nil
	}

	r.logger.Info("kapanma sinyali alındı, devam eden job'lar bekleniyor",
		slog.Duration("grace", r.grace))

	select {
	case <-done:
		r.logger.Info("job'lar durdu")

		return nil
	case <-time.After(r.grace):
	}

	r.logger.Warn("kapanma süresi doldu, devam eden job'lar iptal ediliyor",
		slog.Duration("grace", r.grace))
	cancelJobs()

	select {
	case <-done:
		return nil
	case <-time.After(cancelSlack):
		return fmt.Errorf("%w (%s)", ErrShutdownIncomplete, cancelSlack)
	}
}

// loop, tek bir job'ın zamanlama döngüsü.
//
// ctx  = kapanma sinyali (yeni koşu başlatmayı durdurur)
// jobCtx = koşuların context'i (yalnızca drain süresi dolarsa iptal edilir)
func (r *Runtime) loop(ctx context.Context, jobCtx context.Context, job Job) {
	// İLK KOŞU HEMEN, bir aralık beklemeden: yeniden başlatılan bir worker'ın
	// ilk turu beklemesi, 3.2'de kontrol aralığı kadar kör nokta demekti
	// (deploy başına sessiz bir boşluk).
	if ctx.Err() != nil {
		return
	}

	r.runOnce(jobCtx, job)

	ticker := time.NewTicker(job.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Sinyal ile tick aynı ana denk gelirse yeni koşu başlatılmaz:
			// `select` iki hazır kanaldan rastgele birini seçiyor.
			if ctx.Err() != nil {
				return
			}

			r.runOnce(jobCtx, job)

			// Koşu aralıktan uzun sürdüyse biriken tick DÜŞÜRÜLÜR: gecikmeyi
			// telafi etmek için arka arkaya koşmak, zaten yavaşlamış bir
			// job'ın (ve hedef sistemlerin) üzerine yük bindirirdi.
			select {
			case <-ticker.C:
			default:
			}
		}
	}
}

// runOnce, job'u bir kez çalıştırır; hata ve panikleri yalıtır.
func (r *Runtime) runOnce(ctx context.Context, job Job) {
	startedAt := r.now()

	defer func() {
		// PANİK KURTARILIYOR: bir job'daki nil pointer, o job'u değil TÜM
		// worker'ı (yani bütün monitor'lerin kontrolünü) durdururdu. Ama
		// sessizce değil — stack ile Error seviyesinde loglanıyor, çünkü
		// buraya düşmek her zaman bir bug demek.
		if recovered := recover(); recovered != nil {
			r.logger.Error("job panikledi, worker ayakta kalıyor",
				slog.String("job", job.Name),
				slog.Any("panic", recovered),
				slog.Duration("elapsed", r.now().Sub(startedAt)),
				slog.String("stack", string(debug.Stack())))
		}
	}()

	err := job.Run(ctx)
	elapsed := r.now().Sub(startedAt)

	switch {
	case err != nil && ctx.Err() != nil:
		// Kapanma sırasında iptal edilen koşu bir arıza DEĞİL: bunu "job
		// başarısız" diye loglamak, her yeniden başlatmada sahte bir hata
		// satırı üretmek olurdu (2.2'nin ErrCheckNotRun ayrımıyla aynı fikir).
		r.logger.Info("job kapanma sırasında iptal edildi",
			slog.String("job", job.Name),
			slog.Duration("elapsed", elapsed))
	case err != nil:
		r.logger.Error("job başarısız",
			slog.String("job", job.Name),
			slog.Duration("elapsed", elapsed),
			slog.String("error", err.Error()))
	default:
		r.logger.Debug("job tamamlandı",
			slog.String("job", job.Name),
			slog.Duration("elapsed", elapsed))
	}

	// Aralığından uzun süren bir job, zamanlamanın ARTIK TUTMADIĞI anlamına
	// gelir (3.2'de: kontroller gecikiyor). Sessiz kalmaması gerekiyor.
	if elapsed > job.Interval {
		r.logger.Warn("job aralığından uzun sürdü",
			slog.String("job", job.Name),
			slog.Duration("elapsed", elapsed),
			slog.Duration("interval", job.Interval))
	}
}
