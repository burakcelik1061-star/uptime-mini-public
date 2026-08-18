package worker

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder, testlerin log satırlarını inceleyebilmesi için bir slog handler'ı.
//
// Log İÇERİĞİ burada gerçekten test edilecek bir davranış: bir job'ın hata
// vermesi ile kapanma sırasında iptal edilmesi arasındaki ayrım YALNIZCA loga
// yansıyor (ikisi de süreci düşürmüyor). Ayrım bozulursa her yeniden
// başlatmada sahte hata satırları görürüz.
type recorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())

	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

// find, verilen seviyede ve mesajı `substr` içeren bir kayıt arar.
func (r *recorder) find(level slog.Level, substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, record := range r.records {
		if record.Level == level && strings.Contains(record.Message, substr) {
			return true
		}
	}

	return false
}

func (r *recorder) logger() *slog.Logger { return slog.New(r) }

func TestRegisterRejectsInvalidJobs(t *testing.T) {
	noop := func(context.Context) error { return nil }

	cases := map[string]Job{
		"boş ad":         {Name: "", Interval: time.Second, Run: noop},
		"rezerve ad":     {Name: heartbeatJobName, Interval: time.Second, Run: noop},
		"sıfır aralık":   {Name: "job", Interval: 0, Run: noop},
		"negatif aralık": {Name: "job", Interval: -time.Second, Run: noop},
		"Run alanı boş":  {Name: "job", Interval: time.Second, Run: nil},
	}

	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			runtime := New(Options{Logger: discardLogger()})
			if err := runtime.Register(job); err == nil {
				t.Fatal("geçersiz job kabul edildi")
			}
		})
	}
}

func TestRegisterRejectsDuplicateName(t *testing.T) {
	runtime := New(Options{Logger: discardLogger()})
	job := Job{Name: "aynı", Interval: time.Second, Run: func(context.Context) error { return nil }}

	if err := runtime.Register(job); err != nil {
		t.Fatalf("ilk kayıt başarısız: %v", err)
	}

	if err := runtime.Register(job); err == nil {
		t.Fatal("aynı adla ikinci kayıt kabul edildi")
	}
}

// İş yapmayan ama "çalışıyor" görünen bir worker, monitor'lerin kontrol
// edilmediğinin fark edilmediği durumun tam kendisi.
func TestRunWithoutJobsFails(t *testing.T) {
	runtime := New(Options{Logger: discardLogger()})

	if err := runtime.Run(context.Background()); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("hata = %v, beklenen ErrNoJobs", err)
	}
}

// İlk koşu BİR ARALIK BEKLEMEDEN yapılmalı: aksi hâlde her yeniden başlatma,
// job aralığı kadar (3.2'de kontrol aralığı kadar) kör nokta demek.
func TestRunExecutesJobImmediately(t *testing.T) {
	ran := make(chan struct{}, 1)

	runtime := New(Options{Logger: discardLogger(), ShutdownGrace: time.Second})
	mustRegister(t, runtime, Job{
		Name:     "hemen",
		Interval: time.Hour, // tick beklenirse test zaman aşımına uğrar
		Run: func(context.Context) error {
			select {
			case ran <- struct{}{}:
			default:
			}

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("job açılışta çalışmadı")
	}

	cancel()
	waitRun(t, done, 2*time.Second)
}

// Kapanma sinyali, bir sonraki tick beklenmeden çıkışa yol açmalı: 60 saniyelik
// bir job aralığı, konteyner durdurmayı 60 saniye geciktirmemeli.
func TestRunStopsPromptlyOnCancel(t *testing.T) {
	runtime := New(Options{Logger: discardLogger(), ShutdownGrace: 5 * time.Second})
	mustRegister(t, runtime, Job{
		Name:     "hızlı",
		Interval: 10 * time.Minute,
		Run:      func(context.Context) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(runtime, ctx)

	cancel()
	waitRun(t, done, 2*time.Second)
}

// DEVAM EDEN İŞ DRAIN SÜRESİNCE KESİLMİYOR: 3.2'de yarıda kesilen bir kontrol
// `checker.ErrCheckNotRun` ile kaybedilmiş ölçüm demek. Kapanma sinyali alan
// worker elindeki işi bitirip çıkmalı.
func TestRunLetsInFlightJobFinishWithinGrace(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan error, 1)

	runtime := New(Options{Logger: discardLogger(), ShutdownGrace: 5 * time.Second})
	mustRegister(t, runtime, Job{
		Name:     "yavaş",
		Interval: time.Hour,
		Run: func(ctx context.Context) error {
			close(started)
			time.Sleep(120 * time.Millisecond)
			// İşin sonunda context HÂLÂ canlı olmalı.
			observed <- ctx.Err()

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	<-started
	cancel()

	waitRun(t, done, 3*time.Second)

	select {
	case err := <-observed:
		if err != nil {
			t.Fatalf("devam eden job'ın context'i iptal edilmiş: %v", err)
		}
	default:
		t.Fatal("job tamamlanmadan Run döndü")
	}
}

// Drain süresi dolduğunda job'ın context'i İPTAL EDİLİYOR: sonsuza kadar
// beklemek, kapanmayan bir süreç demek.
func TestRunCancelsJobWhenGraceExpires(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan error, 1)
	logs := &recorder{}

	runtime := New(Options{Logger: logs.logger(), ShutdownGrace: 20 * time.Millisecond})
	mustRegister(t, runtime, Job{
		Name:     "takılan",
		Interval: time.Hour,
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			observed <- ctx.Err()

			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	<-started
	cancel()

	waitRun(t, done, 3*time.Second)

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("job'ın gördüğü hata = %v, beklenen context.Canceled", err)
		}
	default:
		t.Fatal("job iptal edilmedi")
	}

	// Kapanma sırasında iptal edilen koşu HATA olarak loglanmamalı.
	if logs.find(slog.LevelError, "job başarısız") {
		t.Error("kapanma iptali 'job başarısız' olarak loglandı")
	}

	if !logs.find(slog.LevelInfo, "kapanma sırasında iptal edildi") {
		t.Error("kapanma iptali loglanmadı")
	}
}

// context'ini yok sayan bir job süreci sonsuza kadar açık tutmamalı; kapanma
// tamamlanmadıysa süreç bunu HATAYLA bildiriyor (sessizce "kapandım" demiyor).
func TestRunReportsIncompleteShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	runtime := New(Options{Logger: discardLogger(), ShutdownGrace: 10 * time.Millisecond})
	mustRegister(t, runtime, Job{
		Name:     "inatçı",
		Interval: time.Hour,
		Run: func(context.Context) error {
			close(started)
			<-release // context bilinçli olarak yok sayılıyor

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	<-started
	cancel()

	err := waitRunError(t, done, cancelSlack+3*time.Second)
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("hata = %v, beklenen ErrShutdownIncomplete", err)
	}
}

// Bir job'daki panik TÜM worker'ı (yani bütün monitor'lerin kontrolünü)
// durdurmamalı — ama sessizce yutulmamalı da.
func TestRunRecoversPanicAndKeepsGoing(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	secondRun := make(chan struct{})
	logs := &recorder{}

	runtime := New(Options{Logger: logs.logger(), ShutdownGrace: time.Second})
	mustRegister(t, runtime, Job{
		Name:     "panikleyen",
		Interval: 15 * time.Millisecond,
		Run: func(context.Context) error {
			mu.Lock()
			runs++
			current := runs
			mu.Unlock()

			if current == 1 {
				panic("bilerek")
			}

			if current == 2 {
				close(secondRun)
			}

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	select {
	case <-secondRun:
	case <-time.After(3 * time.Second):
		t.Fatal("panik sonrası job bir daha çalışmadı")
	}

	cancel()
	waitRun(t, done, 2*time.Second)

	if !logs.find(slog.LevelError, "job panikledi") {
		t.Error("panik loglanmadı")
	}
}

// Bir job'ın hatası diğer job'ları etkilememeli; hata Error seviyesinde
// loglanmalı.
func TestRunLogsJobErrorAndIsolatesJobs(t *testing.T) {
	healthy := make(chan struct{}, 1)
	// failed, DÜŞEN job'ın gerçekten çalıştığını bildiriyor.
	//
	// Beklemek ŞART: iki job ayrı goroutine'lerde başlıyor ve yalnızca
	// "sağlam" job'ı beklemek, "düşen"in henüz hiç çalışmamış olabileceği bir
	// pencere bırakıyordu. O pencerede gelen `cancel()` döngüyü ilk koşudan
	// ÖNCE durdurur ve hata satırı hiç yazılmazdı — testi yavaş bir makinede
	// (CI) rastgele düşüren yarış buydu.
	failed := make(chan struct{}, 1)
	logs := &recorder{}

	runtime := New(Options{Logger: logs.logger(), ShutdownGrace: time.Second})
	mustRegister(t, runtime, Job{
		Name:     "düşen",
		Interval: 10 * time.Millisecond,
		Run: func(context.Context) error {
			select {
			case failed <- struct{}{}:
			default:
			}

			return errors.New("bilerek hata")
		},
	})
	mustRegister(t, runtime, Job{
		Name:     "sağlam",
		Interval: 10 * time.Millisecond,
		Run: func(context.Context) error {
			select {
			case healthy <- struct{}{}:
			default:
			}

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	select {
	case <-healthy:
	case <-time.After(2 * time.Second):
		t.Fatal("hatalı job diğerini durdurdu")
	}

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("düşen job hiç çalışmadı")
	}

	// `cancel()` devam eden koşuyu KESMİYOR (runOnce sonuna kadar gidiyor) ve
	// `waitRun` tüm döngülerin dönmesini bekliyor; yani buraya gelindiğinde
	// hata satırı yazılmış olmak zorunda.
	cancel()
	waitRun(t, done, 2*time.Second)

	if !logs.find(slog.LevelError, "job başarısız") {
		t.Error("job hatası loglanmadı")
	}
}

// Aralığından uzun süren bir job, zamanlamanın artık tutmadığı anlamına gelir
// (3.2'de: kontroller gecikiyor) — sessiz kalmamalı.
func TestRunWarnsWhenJobOverrunsInterval(t *testing.T) {
	logs := &recorder{}
	slow := make(chan struct{})

	runtime := New(Options{Logger: logs.logger(), ShutdownGrace: time.Second})
	mustRegister(t, runtime, Job{
		Name:     "uzun",
		Interval: 5 * time.Millisecond,
		Run: func(context.Context) error {
			time.Sleep(30 * time.Millisecond)
			select {
			case slow <- struct{}{}:
			default:
			}

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)
	<-slow
	cancel()
	waitRun(t, done, 2*time.Second)

	if !logs.find(slog.LevelWarn, "aralığından uzun sürdü") {
		t.Error("aralık aşımı loglanmadı")
	}
}

// Kalp atışı runtime'ın KENDİ job'u: dosyanın tazelenmesi "runtime tur atıyor"
// demek.
func TestRunWritesHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")

	runtime := New(Options{
		Logger:        discardLogger(),
		ShutdownGrace: time.Second,
		Heartbeat:     Heartbeat{Path: path, Period: 10 * time.Millisecond},
	})
	mustRegister(t, runtime, Job{
		Name:     "boş",
		Interval: time.Hour,
		Run:      func(context.Context) error { return nil },
	})

	if names := runtime.JobNames(); len(names) != 2 || names[1] != heartbeatJobName {
		t.Fatalf("job adları = %v, kalp atışı bekleniyordu", names)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)

	deadline := time.Now().Add(2 * time.Second)
	var first time.Time
	for {
		at, err := ReadHeartbeat(path)
		if err == nil {
			first = at

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("kalp atışı dosyası yazılmadı: %v", err)
		}

		time.Sleep(5 * time.Millisecond)
	}

	// Tazeleniyor mu (tek bir açılış yazması değil)?
	for {
		at, err := ReadHeartbeat(path)
		if err == nil && at.After(first) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("kalp atışı tazelenmedi")
		}

		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	waitRun(t, done, 2*time.Second)
}

// Kalp atışı yolu boşsa dosya yazılmıyor (dosya sistemine yazamayan
// dağıtımlar ve testler için).
func TestRunWithoutHeartbeatPath(t *testing.T) {
	ran := make(chan struct{}, 1)

	runtime := New(Options{Logger: discardLogger(), ShutdownGrace: time.Second})
	mustRegister(t, runtime, Job{
		Name:     "tek",
		Interval: time.Hour,
		Run: func(context.Context) error {
			select {
			case ran <- struct{}{}:
			default:
			}

			return nil
		},
	})

	if names := runtime.JobNames(); len(names) != 1 {
		t.Fatalf("job adları = %v, yalnızca kayıtlı job bekleniyordu", names)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(runtime, ctx)
	<-ran
	cancel()
	waitRun(t, done, 2*time.Second)
}

// Zaten iptal edilmiş bir context ile çağrıldığında hiç koşu başlatılmamalı.
func TestRunWithCancelledContextDoesNotRunJobs(t *testing.T) {
	var mu sync.Mutex
	runs := 0

	runtime := New(Options{Logger: discardLogger(), ShutdownGrace: time.Second})
	mustRegister(t, runtime, Job{
		Name:     "çalışmamalı",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			mu.Lock()
			runs++
			mu.Unlock()

			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runtime.Run(ctx); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if runs != 0 {
		t.Errorf("koşu sayısı = %d, beklenen 0", runs)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func mustRegister(t *testing.T, runtime *Runtime, job Job) {
	t.Helper()

	if err := runtime.Register(job); err != nil {
		t.Fatalf("job kaydedilemedi: %v", err)
	}
}

func runAsync(runtime *Runtime, ctx context.Context) chan error {
	done := make(chan error, 1)

	go func() { done <- runtime.Run(ctx) }()

	return done
}

func waitRun(t *testing.T, done chan error, timeout time.Duration) {
	t.Helper()

	if err := waitRunError(t, done, timeout); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
}

func waitRunError(t *testing.T, done chan error, timeout time.Duration) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("Run %s içinde dönmedi", timeout)

		return nil
	}
}
