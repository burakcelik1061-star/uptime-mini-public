// Package config, süreçlerin (api, worker, migrate) ortam değişkenlerinden
// okuduğu yapılandırmayı tek yerde toplar.
//
// Kural: yapılandırma süreç BAŞLARKEN bir kez okunur ve doğrulanır; kod
// içine `os.Getenv` çağrısı dağıtılmaz. Böylece eksik/bozuk bir değer,
// sürecin ilerleyen bir anında değil ilk saniyesinde ortaya çıkar.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
)

// Environment, uygulamanın çalıştığı ortam.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvTest        Environment = "test"
	EnvProduction  Environment = "production"
)

// DatabaseConfig, Postgres bağlantı ayarları.
type DatabaseConfig struct {
	URL string
	// api ve worker AYRI süreçler, yani AYRI havuzlar açıyor. Toplamın
	// Postgres'in `max_connections` değerini aşmaması için ikisine farklı
	// üst sınır verilebiliyor: api istek başına kısa sorgular yapar, worker
	// ise eşzamanlı kontrolleri yürütür.
	MaxConns int32
}

// JWTConfig, access token imzalama ayarları.
type JWTConfig struct {
	AccessSecret []byte
	AccessTTL    time.Duration
}

// RefreshConfig, refresh token ayarları.
//
// Ortam değişkeni `JWT_REFRESH_SECRET` değil `REFRESH_TOKEN_SECRET`: refresh
// token bir JWT DEĞİL (opak rastgele değer, bkz. internal/auth/refresh.go),
// bu anahtar da imzalama değil HMAC özet anahtarı. Yanlış ön ek, kodu 6 ay
// sonra okuyanı yanıltırdı.
type RefreshConfig struct {
	Secret []byte
	TTL    time.Duration
}

// APIConfig, HTTP API sürecinin yapılandırması.
type APIConfig struct {
	Env             Environment
	Port            int
	Database        DatabaseConfig
	WebOrigins      []string
	PublicBaseURL   string
	ShutdownTimeout time.Duration
	// TrustedProxies, `X-Forwarded-For` başlığına güvenilecek KAYNAK adresler
	// (IP veya CIDR listesi). BOŞ = hiçbir başlık okunmuyor ve hız sınırının
	// kimliği ham TCP adresi kalıyor — 0.4'ün `middleware.RealIP` kararı.
	//
	// Doldurmak gerekiyorsa sebebi şu: public status sayfası ve abonelik
	// uçları 5.1/6.4'ün kararı gereği ziyaretçinin tarayıcısından değil NEXT
	// SUNUCUSUNDAN çağrılıyor, yani api tüm ziyaretçileri tek bir adres olarak
	// görüyor. Değer ayrıştırma/doğrulama `httpapi.ParseTrustedProxies` ile
	// boot'ta yapılıyor (bkz. cmd/api/main.go): kural ile onu uygulayan kod
	// aynı pakette kalsın.
	TrustedProxies []string
	// DialPolicy, kullanıcının yazdığı adreslere giden isteklerin SSRF
	// koruması (7.4). api'de manuel kontrol ve webhook testi bu politikayı
	// kullanıyor.
	DialPolicy safedial.Policy
	// DevTools, geliştirme araçlarının (8.2'nin posta kutusu) açık olup
	// olmadığı. Bkz. devToolsEnabled — kapı `dialPolicy` ile AYNI şartı
	// taşıyor: APP_ENV AÇIKÇA development|test olmalı.
	DevTools bool
	JWT      JWTConfig
	Refresh  RefreshConfig
	// Warnings, yapılandırma geçerli ama dikkat çekilmesi gereken durumlar.
	// Config yüklenirken logger henüz kurulmamış oluyor; uyarılar burada
	// biriktirilip çağıran tarafından loglanıyor.
	Warnings []string
}

// WorkerConfig, arka plan worker sürecinin yapılandırması.
type WorkerConfig struct {
	Env      Environment
	Database DatabaseConfig
	// DBProbePeriod, veritabanı erişilebilirlik yoklamasının aralığı.
	DBProbePeriod time.Duration
	// SchedulerPeriod, "sırası gelen monitor var mı" turunun aralığı.
	SchedulerPeriod time.Duration
	// MaxConcurrentChecks, aynı anda çalışabilecek kontrol sayısı.
	MaxConcurrentChecks int
	// RollupPeriod, saatlik/günlük özetleri tazeleyen turun aralığı.
	RollupPeriod time.Duration
	// RetentionPeriod, budama turunun aralığı.
	RetentionPeriod time.Duration
	// NotificationPeriod, bekleyen bildirimleri gönderen turun aralığı (6.3).
	NotificationPeriod time.Duration
	// PublicBaseURL, e-postadaki onay/iptal linklerinin kökü (6.4). api'nin
	// değil ARAYÜZÜN adresi: linke tıklayan kişi bir JSON cevabı değil, ne
	// olduğunu anlatan bir sayfa görmeli.
	PublicBaseURL string
	// RawRetentionDays, ham kontrol sonuçlarının saklama süresi (gün).
	RawRetentionDays int
	Heartbeat        HeartbeatConfig
	// ShutdownTimeout, kapanma sinyalinden sonra devam eden job'lara tanınan
	// süre (api'de açık isteklere tanınan sürenin karşılığı).
	ShutdownTimeout time.Duration
	// DialPolicy, zamanlanmış kontrollerin ve webhook gönderiminin SSRF
	// koruması (7.4).
	DialPolicy safedial.Policy
	// DevTools, geliştirme araçlarının açık olup olmadığı (8.2). worker'ın da
	// bilmesi gerekiyor: kesinti bildirimleri worker'dan gidiyor ve posta
	// kutusunda GÖRÜNMELİ — yoksa ekran gerçeğin yalnızca yarısını gösterirdi.
	DevTools bool
	// Warnings, yapılandırma geçerli ama dikkat çekilmesi gereken durumlar
	// (api'deki karşılığıyla aynı; logger config yüklenirken henüz yok).
	Warnings []string
}

// HeartbeatConfig, worker'ın canlılık sinyali ayarları.
//
// worker'ın HTTP portu YOK, yani api gibi `/health` ile yoklanamıyor; canlılık
// sinyali bir dosyaya yazılan zaman damgası. `worker healthcheck` alt komutu
// (ve compose'daki healthcheck) bu dosyanın tazeliğine bakıyor.
type HeartbeatConfig struct {
	Path   string
	Period time.Duration
}

// MigrateConfig, migration binary'sinin yapılandırması.
type MigrateConfig struct {
	Env         Environment
	DatabaseURL string
}

const (
	// canny-mini (aynı makinedeki 1. proje) 3000/3333 kullanıyor; bu proje
	// bilinçli olarak 3100/3400'de çalışıyor ki ikisi aynı anda ayakta
	// olabilsin.
	defaultAPIPort         = 3400
	defaultShutdownTimeout = 10 * time.Second
	defaultAPIMaxConns     = 10
	defaultWorkerMaxConns  = 5

	// Veritabanı yoklaması 30 saniyede bir: amaç kesintiyi milisaniyede
	// yakalamak değil, veritabanının ne zaman kaybedilip geri geldiğini loga
	// düşürmek. Daha sık yoklamak boşta duran bir worker'a boş yük bindirirdi.
	defaultDBProbePeriod = 30 * time.Second

	// Kalp atışı 10 saniyede bir yazılıyor: tolerans katsayısıyla (3×) birlikte
	// donmuş bir worker en fazla 30 saniyede unhealthy görünüyor.
	defaultHeartbeatPeriod = 10 * time.Second

	// Scheduler turu 5 saniyede bir: monitor'ün en küçük kontrol aralığı 30
	// saniye (şemadaki kısıt), yani 5 saniyelik tur "sırası geleni" en fazla 5
	// saniye gecikmeyle yakalıyor. Daha sık tur, boşta dönen sorgu demek;
	// daha seyrek tur, kullanıcının seçtiği aralığı gözle görülür kaydırırdı.
	defaultSchedulerPeriod = 5 * time.Second

	// Eşzamanlı kontrol sınırı. Değer `internal/scheduler`ın varsayılanıyla
	// aynı ama BURADA ayrıca tanımlı: yapılandırma paketi bir iş mantığı
	// paketine bağımlı olmasın (bağımlılık yönü tek taraflı kalsın).
	defaultMaxConcurrentChecks = 10

	// Özetler 5 dakikada bir tazeleniyor: devam eden saat de yeniden
	// hesaplandığı için (idempotent upsert) grafik en fazla 5 dakika geriden
	// geliyor. Daha sık çalıştırmak aynı satırları boşuna yeniden hesaplardı.
	defaultRollupPeriod = 5 * time.Minute

	// Budama saatte bir: partition penceresini ileri kaydırmak ve günlük
	// sınırları kontrol etmek için fazlasıyla sık. Günde bir çalıştırmak,
	// worker'ın gün dönümünde kapalı olduğu bir günü atlardı.
	defaultRetentionPeriod = time.Hour

	// Bildirim turu 15 saniyede bir: bir kesintiden haberdar olmanın kabul
	// edilebilir gecikmesi bu mertebede ve tur boştayken iki ucuz sorgu
	// (kuyruğa yaz + sırası geleni sahiplen) demek. Daha sık çalıştırmak
	// kesintiyi daha erken fark ettirmiyor — onu 3.2'nin kontrol aralığı
	// belirliyor.
	defaultNotificationPeriod = 15 * time.Second

	// Arayüzün local adresi (0.2'de seçilen port).
	defaultPublicBaseURL = "http://localhost:3100"

	// Ham veri saklama süresi (0.1 §6.3). Üst sınır: partition başına bir
	// tablo olduğu için 365 günden fazlası katalogda yüzlerce tablo demek.
	defaultRawRetentionDays = 30
	maxRawRetentionDays     = 365

	// maxConcurrentChecksLimit, yapılandırmanın üst sınırı. Sınırsız bırakmak,
	// tek bir yazım hatasıyla (10000) makineyi ve hedef sistemleri boğan bir
	// worker demekti.
	maxConcurrentChecksLimit = 500

	// Kalp atışı dosyasının adı. Dizin `os.TempDir()`den geliyor: konteynerde
	// /tmp (uptime kullanıcısının yazabildiği tek yer), Windows'ta kullanıcının
	// geçici dizini — yani klonlayan herkeste ayar gerektirmeden çalışıyor.
	heartbeatFileName = "uptime-mini-worker.heartbeat"

	// Access token kısa ömürlü: çalınan bir token'ın penceresi dar olsun.
	// Uzun oturum, Faz 1.2'nin refresh token akışıyla sağlanacak.
	defaultAccessTTL = 15 * time.Minute
	maxAccessTTL     = time.Hour

	// Refresh token uzun ömürlü: kullanıcı her 15 dakikada bir yeniden giriş
	// yapmasın. Her kullanımda döndüğü (rotation) ve iptal edilebildiği için
	// uzun ömür burada access token'daki riski taşımıyor.
	defaultRefreshTTL = 30 * 24 * time.Hour
	maxRefreshTTL     = 365 * 24 * time.Hour

	// minSecretLength, imzalama anahtarının en az uzunluğu.
	// `openssl rand -hex 32` çıktısı 64 karakterdir; 32 karakterin altı
	// kaba kuvvetle denenebilecek kadar kısa demektir.
	minSecretLength = 32
)

// LoadAPI, api sürecinin yapılandırmasını ortamdan okur.
func LoadAPI() (APIConfig, error) {
	env, envExplicit, err := environmentFromEnv()
	if err != nil {
		return APIConfig{}, err
	}

	port, err := intFromEnv("API_PORT", defaultAPIPort)
	if err != nil {
		return APIConfig{}, err
	}

	shutdown, err := durationFromEnv("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return APIConfig{}, err
	}

	database, err := loadDatabase("API_DB_MAX_CONNS", defaultAPIMaxConns)
	if err != nil {
		return APIConfig{}, err
	}

	if port < 1 || port > 65535 {
		return APIConfig{}, fmt.Errorf("API_PORT geçersiz: %d", port)
	}

	jwt, refresh, warnings, err := loadTokens(env, envExplicit)
	if err != nil {
		return APIConfig{}, err
	}

	policy, policyWarnings, err := dialPolicy(env, envExplicit)
	if err != nil {
		return APIConfig{}, err
	}
	warnings = append(warnings, policyWarnings...)

	return APIConfig{
		Env:             env,
		Port:            port,
		Database:        database,
		WebOrigins:      splitAndTrim(os.Getenv("WEB_ORIGINS")),
		PublicBaseURL:   publicBaseURL(),
		ShutdownTimeout: shutdown,
		TrustedProxies:  splitAndTrim(os.Getenv("TRUSTED_PROXIES")),
		DialPolicy:      policy,
		DevTools:        devToolsEnabled(env, envExplicit),
		JWT:             jwt,
		Refresh:         refresh,
		Warnings:        warnings,
	}, nil
}

// LoadWorker, worker sürecinin yapılandırmasını ortamdan okur.
func LoadWorker() (WorkerConfig, error) {
	env, envExplicit, err := environmentFromEnv()
	if err != nil {
		return WorkerConfig{}, err
	}

	probePeriod, err := durationFromEnv("WORKER_DB_PROBE_PERIOD", defaultDBProbePeriod)
	if err != nil {
		return WorkerConfig{}, err
	}

	schedulerPeriod, err := durationFromEnv("WORKER_SCHEDULER_PERIOD", defaultSchedulerPeriod)
	if err != nil {
		return WorkerConfig{}, err
	}

	maxConcurrentChecks, err := intFromEnv("WORKER_MAX_CONCURRENT_CHECKS", defaultMaxConcurrentChecks)
	if err != nil {
		return WorkerConfig{}, err
	}

	rollupPeriod, err := durationFromEnv("WORKER_ROLLUP_PERIOD", defaultRollupPeriod)
	if err != nil {
		return WorkerConfig{}, err
	}

	retentionPeriod, err := durationFromEnv("WORKER_RETENTION_PERIOD", defaultRetentionPeriod)
	if err != nil {
		return WorkerConfig{}, err
	}

	notificationPeriod, err := durationFromEnv("WORKER_NOTIFICATION_PERIOD", defaultNotificationPeriod)
	if err != nil {
		return WorkerConfig{}, err
	}

	rawRetentionDays, err := intFromEnv("RAW_RETENTION_DAYS", defaultRawRetentionDays)
	if err != nil {
		return WorkerConfig{}, err
	}

	heartbeat, err := LoadWorkerHeartbeat()
	if err != nil {
		return WorkerConfig{}, err
	}

	shutdown, err := durationFromEnv("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return WorkerConfig{}, err
	}

	database, err := loadDatabase("WORKER_DB_MAX_CONNS", defaultWorkerMaxConns)
	if err != nil {
		return WorkerConfig{}, err
	}

	if probePeriod <= 0 {
		return WorkerConfig{}, fmt.Errorf("WORKER_DB_PROBE_PERIOD pozitif olmalı: %s", probePeriod)
	}

	if schedulerPeriod <= 0 {
		return WorkerConfig{}, fmt.Errorf("WORKER_SCHEDULER_PERIOD pozitif olmalı: %s", schedulerPeriod)
	}

	if rollupPeriod <= 0 {
		return WorkerConfig{}, fmt.Errorf("WORKER_ROLLUP_PERIOD pozitif olmalı: %s", rollupPeriod)
	}

	if retentionPeriod <= 0 {
		return WorkerConfig{}, fmt.Errorf("WORKER_RETENTION_PERIOD pozitif olmalı: %s", retentionPeriod)
	}

	if notificationPeriod <= 0 {
		return WorkerConfig{}, fmt.Errorf("WORKER_NOTIFICATION_PERIOD pozitif olmalı: %s", notificationPeriod)
	}

	// Alt sınır 1 gün: ham veriyi aynı gün silmek, monitor detay sayfasını
	// (2.4) boşaltmak demekti.
	if rawRetentionDays < 1 || rawRetentionDays > maxRawRetentionDays {
		return WorkerConfig{}, fmt.Errorf("RAW_RETENTION_DAYS 1 ile %d arasında olmalı: %d",
			maxRawRetentionDays, rawRetentionDays)
	}

	if maxConcurrentChecks < 1 || maxConcurrentChecks > maxConcurrentChecksLimit {
		return WorkerConfig{}, fmt.Errorf("WORKER_MAX_CONCURRENT_CHECKS 1 ile %d arasında olmalı: %d",
			maxConcurrentChecksLimit, maxConcurrentChecks)
	}

	if shutdown < 0 {
		return WorkerConfig{}, fmt.Errorf("SHUTDOWN_TIMEOUT negatif olamaz: %s", shutdown)
	}

	policy, warnings, err := dialPolicy(env, envExplicit)
	if err != nil {
		return WorkerConfig{}, err
	}

	return WorkerConfig{
		Env:                 env,
		Database:            database,
		DBProbePeriod:       probePeriod,
		SchedulerPeriod:     schedulerPeriod,
		MaxConcurrentChecks: maxConcurrentChecks,
		RollupPeriod:        rollupPeriod,
		RetentionPeriod:     retentionPeriod,
		NotificationPeriod:  notificationPeriod,
		PublicBaseURL:       publicBaseURL(),
		RawRetentionDays:    rawRetentionDays,
		Heartbeat:           heartbeat,
		ShutdownTimeout:     shutdown,
		DialPolicy:          policy,
		DevTools:            devToolsEnabled(env, envExplicit),
		Warnings:            warnings,
	}, nil
}

// devToolsEnabled, geliştirme araçlarının açılıp açılmayacağını söyler (8.2).
//
// `envExplicit` ŞARTI 1.1'İN İMZALAMA ANAHTARI ve 7.4/7.5'İN SSRF KAPISIYLA
// AYNI: `environmentFromEnv` APP_ENV verilmediğinde `development` döndürüyor,
// yani yalnızca çözülmüş değere bakmak "APP_ENV geçmeyi unutan bir dağıtımda
// geliştirici posta kutusunun kendiliğinden açılması" demekti. Posta kutusu
// onay/davet/parola sıfırlama linklerini DÜZ METİN taşıyor — açık kalması,
// o linkleri okuyabilen herkese hesap devralma yetkisi vermek olurdu.
//
// Kapının çıktısı bir BAYRAK, bir ortam değeri değil: çağıranlar "hangi
// ortamdayız" sorusunu yeniden sormuyor, kararı burada verilmiş buluyor
// (dialPolicy'nin `safedial.Policy` döndürmesiyle aynı biçim).
func devToolsEnabled(env Environment, envExplicit bool) bool {
	return envExplicit && (env == EnvDevelopment || env == EnvTest)
}

// LoadWorkerHeartbeat, canlılık sinyali ayarlarını okur.
//
// AYRI bir yükleyici olması bilinçli: `worker healthcheck` alt komutu bu iki
// değerden başka hiçbir şeye ihtiyaç duymuyor. LoadWorker'ı çağırsaydı
// DATABASE_URL zorunlu olurdu ve canlılık kontrolü, veritabanı
// yapılandırmasının varlığına bağlanırdı — oysa liveness'ın veritabanıyla
// ilgisi yok (0.3'ün `/health` ile `/ready` ayrımı).
func LoadWorkerHeartbeat() (HeartbeatConfig, error) {
	period, err := durationFromEnv("WORKER_HEARTBEAT_PERIOD", defaultHeartbeatPeriod)
	if err != nil {
		return HeartbeatConfig{}, err
	}

	if period <= 0 {
		return HeartbeatConfig{}, fmt.Errorf("WORKER_HEARTBEAT_PERIOD pozitif olmalı: %s", period)
	}

	path := strings.TrimSpace(os.Getenv("WORKER_HEARTBEAT_FILE"))
	if path == "" {
		path = filepath.Join(os.TempDir(), heartbeatFileName)
	}

	return HeartbeatConfig{Path: path, Period: period}, nil
}

// LoadMigrate, migration binary'sinin yapılandırmasını ortamdan okur.
func LoadMigrate() (MigrateConfig, error) {
	env, _, err := environmentFromEnv()
	if err != nil {
		return MigrateConfig{}, err
	}

	url, err := requiredDatabaseURL()
	if err != nil {
		return MigrateConfig{}, err
	}

	return MigrateConfig{Env: env, DatabaseURL: url}, nil
}

func loadDatabase(maxConnsKey string, defaultMaxConns int) (DatabaseConfig, error) {
	url, err := requiredDatabaseURL()
	if err != nil {
		return DatabaseConfig{}, err
	}

	maxConns, err := intFromEnv(maxConnsKey, defaultMaxConns)
	if err != nil {
		return DatabaseConfig{}, err
	}

	if maxConns < 1 {
		return DatabaseConfig{}, fmt.Errorf("%s en az 1 olmalı: %d", maxConnsKey, maxConns)
	}

	return DatabaseConfig{URL: url, MaxConns: int32(maxConns)}, nil
}

// requiredDatabaseURL, bağlantı dizesini okur ve en temel biçim kontrolünü
// yapar. Faz 0.3'ten itibaren HER ortamda zorunlu: veritabanı olmadan hiçbir
// süreç anlamlı iş yapamıyor, eksikliği boot anında görünmeli.
func requiredDatabaseURL() (string, error) {
	url := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if url == "" {
		return "", fmt.Errorf("DATABASE_URL tanımlı değil (bkz. .env.example)")
	}

	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		return "", fmt.Errorf("DATABASE_URL postgres:// ile başlamalı")
	}

	return url, nil
}

// environmentFromEnv, APP_ENV'i çözümler.
//
// İkinci dönüş değeri "AÇIKÇA verildi mi" sorusunu cevaplıyor ve gerçekten
// önemli: imzalama anahtarının otomatik üretimi yalnızca APP_ENV açıkça
// development/test iken yapılıyor. APP_ENV'i set etmeyi unutan bir üretim
// dağıtımının, varsayılan "development" üzerinden sessizce rastgele
// anahtarla çalışması engellenmeli.
//
// Bilinmeyen bir değer (ör. "prod", "staging") HATA: sessizce development'a
// düşmek, üretimde fark edilmeyen bir yapılandırma hatasıdır.
func environmentFromEnv() (Environment, bool, error) {
	raw := strings.TrimSpace(os.Getenv("APP_ENV"))

	switch raw {
	case "":
		return EnvDevelopment, false, nil
	case string(EnvDevelopment):
		return EnvDevelopment, true, nil
	case string(EnvTest):
		return EnvTest, true, nil
	case string(EnvProduction):
		return EnvProduction, true, nil
	default:
		return "", false, fmt.Errorf("APP_ENV geçersiz: %q (development|test|production)", raw)
	}
}

// placeholderSecrets, .env.example'daki ve benzeri "doldurulacak" değerler.
// Bilinen bir imzalama anahtarı = tam kimlik taklidi; bu değerler HİÇBİR
// ortamda anahtar olarak kullanılmıyor.
var placeholderSecrets = []string{
	"generate_me",
	"generate-me",
	"changeme",
	"change_me",
	"change-me",
	"placeholder",
	"your-secret",
	"your_secret",
}

// loadTokens, access ve refresh token yapılandırmasını okur.
//
// Kural (bkz. docs/ adım dokümanları):
//   - Verilmiş bir anahtar varsa her ortamda doğrulanır: placeholder olamaz,
//     minSecretLength'ten kısa olamaz, iki anahtar aynı olamaz.
//   - Anahtar yoksa/placeholder ise: APP_ENV AÇIKÇA development veya test ise
//     geçici bir anahtar üretilir (ve yüksek sesle uyarılır), aksi hâlde
//     süreç BOOT ETMEZ.
func loadTokens(env Environment, envExplicit bool) (JWTConfig, RefreshConfig, []string, error) {
	accessTTL, err := durationFromEnv("JWT_ACCESS_TTL", defaultAccessTTL)
	if err != nil {
		return JWTConfig{}, RefreshConfig{}, nil, err
	}
	if accessTTL <= 0 || accessTTL > maxAccessTTL {
		return JWTConfig{}, RefreshConfig{}, nil, fmt.Errorf("JWT_ACCESS_TTL 0 ile %s arasında olmalı: %s", maxAccessTTL, accessTTL)
	}

	refreshTTL, err := durationFromEnv("REFRESH_TOKEN_TTL", defaultRefreshTTL)
	if err != nil {
		return JWTConfig{}, RefreshConfig{}, nil, err
	}
	if refreshTTL <= 0 || refreshTTL > maxRefreshTTL {
		return JWTConfig{}, RefreshConfig{}, nil, fmt.Errorf("REFRESH_TOKEN_TTL 0 ile %s arasında olmalı: %s", maxRefreshTTL, refreshTTL)
	}

	// Refresh token'ın access token'dan kısa yaşaması, uzun oturum fikrini
	// anlamsız kılardı — yapılandırma hatası olarak ele alınıyor.
	if refreshTTL <= accessTTL {
		return JWTConfig{}, RefreshConfig{}, nil, fmt.Errorf("REFRESH_TOKEN_TTL (%s), JWT_ACCESS_TTL'den (%s) uzun olmalı", refreshTTL, accessTTL)
	}

	var warnings []string

	access, accessWarn, err := loadSecret("JWT_ACCESS_SECRET", env, envExplicit)
	if err != nil {
		return JWTConfig{}, RefreshConfig{}, nil, err
	}
	warnings = appendNonEmpty(warnings, accessWarn)

	refresh, refreshWarn, err := loadSecret("REFRESH_TOKEN_SECRET", env, envExplicit)
	if err != nil {
		return JWTConfig{}, RefreshConfig{}, nil, err
	}
	warnings = appendNonEmpty(warnings, refreshWarn)

	// Aynı anahtar iki farklı amaçta kullanılırsa, bir yerdeki zafiyet
	// diğerini de düşürür; ayrı anahtarlar bu bağı kesiyor.
	if string(access) == string(refresh) {
		return JWTConfig{}, RefreshConfig{}, nil, fmt.Errorf("JWT_ACCESS_SECRET ve REFRESH_TOKEN_SECRET aynı olamaz")
	}

	return JWTConfig{AccessSecret: access, AccessTTL: accessTTL},
		RefreshConfig{Secret: refresh, TTL: refreshTTL},
		warnings, nil
}

func loadSecret(key string, env Environment, envExplicit bool) ([]byte, string, error) {
	raw := strings.TrimSpace(os.Getenv(key))

	if raw != "" && !isPlaceholderSecret(raw) {
		if len(raw) < minSecretLength {
			return nil, "", fmt.Errorf("%s en az %d karakter olmalı (openssl rand -hex 32)", key, minSecretLength)
		}

		return []byte(raw), "", nil
	}

	// Buraya gelindiyse kullanılabilir bir anahtar YOK.
	canGenerate := envExplicit && (env == EnvDevelopment || env == EnvTest)
	if !canGenerate {
		if raw == "" {
			return nil, "", fmt.Errorf("%s tanımlı değil; APP_ENV=%q iken otomatik üretim yapılmaz — `openssl rand -hex 32` çıktısını .env'e yaz", key, os.Getenv("APP_ENV"))
		}

		return nil, "", fmt.Errorf("%s bir placeholder (%q); APP_ENV=%q iken otomatik üretim yapılmaz — `openssl rand -hex 32` çıktısını .env'e yaz", key, raw, os.Getenv("APP_ENV"))
	}

	secret, err := randomSecret()
	if err != nil {
		return nil, "", fmt.Errorf("%s üretilemedi: %w", key, err)
	}

	warning := fmt.Sprintf("%s için geçici imzalama anahtarı üretildi — api yeniden başlarsa tüm oturumlar düşer; "+
		"kalıcı olması için .env'e `openssl rand -hex 32` çıktısını yaz.", key)

	return secret, warning, nil
}

func isPlaceholderSecret(raw string) bool {
	lower := strings.ToLower(raw)
	for _, marker := range placeholderSecrets {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	return false
}

// randomSecret, `openssl rand -hex 32` ile aynı güçte (32 byte entropi)
// bir anahtar üretir.
func randomSecret() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}

	return []byte(hex.EncodeToString(buf)), nil
}

func appendNonEmpty(list []string, value string) []string {
	if value == "" {
		return list
	}

	return append(list, value)
}

func intFromEnv(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s sayı olmalı: %q", key, raw)
	}

	return value, nil
}

func durationFromEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s süre olmalı (ör. 30s, 2m): %q", key, raw)
	}

	return value, nil
}

// publicBaseURL, arayüzün dışa görünen adresi (6.4'ün e-posta linkleri).
//
// VARSAYILANI VAR: depoyu klonlayan biri hiçbir .env dosyası olmadan
// çalıştırabilmeli (GÜNCEL DURUM §3). `WEB_ORIGINS`in ilk değerine
// DÜŞMÜYOR — o liste CORS'un konusu ve birden çok origin taşıyabilir;
// e-posta linkinin tek ve kesin bir adresi olmalı.
func publicBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}

	return defaultPublicBaseURL
}

func splitAndTrim(raw string) []string {
	if raw == "" {
		return nil
	}

	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// boolFromEnv, "true/1/yes" gibi değerleri okur.
//
// Tanınmayan bir değer HATA: `CHECKER_ALLOW_PRIVATE_TARGETS=yes please`
// yazan biri sessizce "false" almamalı — bu bayrak bir GÜVENLİK kapısı ve
// sessiz varsayılan, kapının hangi tarafta olduğunu belirsiz bırakırdı.
func boolFromEnv(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s bir doğru/yanlış değeri olmalı (true|false): %q", key, raw)
	}

	return value, nil
}

// dialPolicy, SSRF korumasının politikasını üretir (7.4).
//
// KAPI YALNIZCA APP_ENV **AÇIKÇA** DEVELOPMENT VEYA TEST İKEN AÇILABİLİYOR:
// yerelde monitor hedefi çoğu zaman `localhost` (7.2'nin E2E'si tam olarak öyle
// çalışıyor), ama üretimde aynı bayrağı set etmek HİÇBİR ŞEY değiştirmiyor —
// istek yok sayılıyor ve bu bir UYARI olarak loglanıyor.
//
// `envExplicit` ŞARTI 1.1'İN İMZALAMA ANAHTARI DİSİPLİNİNİN AYNISI ve bilerek
// buraya da kondu: `environmentFromEnv` APP_ENV verilmediğinde `development`
// döndürüyor, yani yalnızca çözülmüş değere bakmak "APP_ENV'i geçmeyi unutan
// bir dağıtımda tek bir ortam değişkeninin korumayı tamamen kapatması"
// demekti. 1.1 anahtar üretiminde o kapıyı kapatmıştı; burası açık kalmıştı.
func dialPolicy(env Environment, envExplicit bool) (safedial.Policy, []string, error) {
	allow, err := boolFromEnv("CHECKER_ALLOW_PRIVATE_TARGETS", false)
	if err != nil {
		return safedial.Policy{}, nil, err
	}

	if !allow {
		return safedial.Policy{}, nil, nil
	}

	if !envExplicit || (env != EnvDevelopment && env != EnvTest) {
		return safedial.Policy{}, []string{
			"CHECKER_ALLOW_PRIVATE_TARGETS yok sayıldı: iç ağa erişim yalnızca " +
				"APP_ENV AÇIKÇA development|test iken açılabilir (SSRF koruması)",
		}, nil
	}

	return safedial.Policy{AllowPrivate: true}, []string{
		"SSRF koruması GEVŞETİLDİ: iç ağ/loopback hedeflerine izin veriliyor " +
			"(CHECKER_ALLOW_PRIVATE_TARGETS, yalnızca geliştirme/test)",
	}, nil
}
