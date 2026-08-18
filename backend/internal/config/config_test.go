package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testDatabaseURL = "postgres://uptime:uptime@localhost:5433/uptime_mini"
	// `openssl rand -hex 32` çıktısı uzunluğunda (64 karakter) değerler.
	testAccessSecret  = "1f0a7c4b9d2e6f8a3b5c7d9e1f2a4b6c8d0e2f4a6b8c0d2e4f6a8b0c2d4e6f8a"
	testRefreshSecret = "9e7d5c3b1a0f8e6d4c2b0a9f7e5d3c1b0a8f6e4d2c0b9a7f5e3d1c0b8a6f4e2d"
)

// setValidEnv, testlerin ortak başlangıç noktası: geçerli bir yapılandırma.
// Her test yalnızca ölçtüğü değişkeni bozuyor.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", testDatabaseURL)
	t.Setenv("APP_ENV", "")
	t.Setenv("API_PORT", "")
	t.Setenv("WEB_ORIGINS", "")
	t.Setenv("API_DB_MAX_CONNS", "")
	t.Setenv("WORKER_DB_MAX_CONNS", "")
	t.Setenv("WORKER_DB_PROBE_PERIOD", "")
	t.Setenv("WORKER_SCHEDULER_PERIOD", "")
	t.Setenv("WORKER_MAX_CONCURRENT_CHECKS", "")
	t.Setenv("WORKER_ROLLUP_PERIOD", "")
	t.Setenv("WORKER_RETENTION_PERIOD", "")
	t.Setenv("RAW_RETENTION_DAYS", "")
	t.Setenv("WORKER_HEARTBEAT_PERIOD", "")
	t.Setenv("WORKER_HEARTBEAT_FILE", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("CHECKER_ALLOW_PRIVATE_TARGETS", "")
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("JWT_ACCESS_SECRET", testAccessSecret)
	t.Setenv("REFRESH_TOKEN_SECRET", testRefreshSecret)
	t.Setenv("JWT_ACCESS_TTL", "")
	t.Setenv("REFRESH_TOKEN_TTL", "")
}

func TestLoadAPIDefaults(t *testing.T) {
	setValidEnv(t)

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.Port != defaultAPIPort {
		t.Errorf("port = %d, beklenen %d", cfg.Port, defaultAPIPort)
	}
	if cfg.Env != EnvDevelopment {
		t.Errorf("env = %q, beklenen %q", cfg.Env, EnvDevelopment)
	}
	if cfg.Database.URL != testDatabaseURL {
		t.Errorf("database url = %q", cfg.Database.URL)
	}
	if cfg.Database.MaxConns != defaultAPIMaxConns {
		t.Errorf("maxConns = %d, beklenen %d", cfg.Database.MaxConns, defaultAPIMaxConns)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("shutdown = %s, beklenen %s", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
}

// Faz 0.3'ten itibaren veritabanı HER ortamda zorunlu: eksikliği boot anında
// görünmeli, ilk sorguda değil.
func TestDatabaseURLIsRequiredEverywhere(t *testing.T) {
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "")

	if _, err := LoadAPI(); err == nil {
		t.Error("api: DATABASE_URL'siz boot edildi, hata bekleniyordu")
	}
	if _, err := LoadWorker(); err == nil {
		t.Error("worker: DATABASE_URL'siz boot edildi, hata bekleniyordu")
	}
	if _, err := LoadMigrate(); err == nil {
		t.Error("migrate: DATABASE_URL'siz çalıştı, hata bekleniyordu")
	}
}

func TestDatabaseURLSchemeIsValidated(t *testing.T) {
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "mysql://uptime:uptime@localhost:3306/uptime_mini")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("postgres olmayan bağlantı dizesi kabul edildi")
	}
}

func TestLoadAPIRejectsInvalidPort(t *testing.T) {
	setValidEnv(t)
	t.Setenv("API_PORT", "üçbin")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("sayı olmayan API_PORT kabul edildi, hata bekleniyordu")
	}
}

func TestLoadAPIRejectsOutOfRangePort(t *testing.T) {
	setValidEnv(t)
	t.Setenv("API_PORT", "70000")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("aralık dışı API_PORT kabul edildi, hata bekleniyordu")
	}
}

func TestLoadAPIRejectsZeroMaxConns(t *testing.T) {
	setValidEnv(t)
	t.Setenv("API_DB_MAX_CONNS", "0")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("sıfır havuz boyutu kabul edildi, hata bekleniyordu")
	}
}

func TestLoadAPIParsesWebOrigins(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WEB_ORIGINS", " http://localhost:3100 , ,http://127.0.0.1:3100 ")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	want := []string{"http://localhost:3100", "http://127.0.0.1:3100"}
	if len(cfg.WebOrigins) != len(want) {
		t.Fatalf("origin sayısı = %d, beklenen %d (%v)", len(cfg.WebOrigins), len(want), cfg.WebOrigins)
	}
	for i := range want {
		if cfg.WebOrigins[i] != want[i] {
			t.Errorf("origin[%d] = %q, beklenen %q", i, cfg.WebOrigins[i], want[i])
		}
	}
}

// api ve worker ayrı havuzlar açıyor; varsayılanları da ayrı olmalı.
func TestWorkerHasItsOwnPoolDefault(t *testing.T) {
	setValidEnv(t)

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.Database.MaxConns != defaultWorkerMaxConns {
		t.Errorf("worker maxConns = %d, beklenen %d", cfg.Database.MaxConns, defaultWorkerMaxConns)
	}
	if defaultWorkerMaxConns == defaultAPIMaxConns {
		t.Error("api ve worker varsayılan havuz boyutları aynı — ayrı olmalı")
	}
}

func TestLoadWorkerParsesPeriods(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WORKER_DB_PROBE_PERIOD", "45s")
	t.Setenv("WORKER_SCHEDULER_PERIOD", "2s")
	t.Setenv("WORKER_MAX_CONCURRENT_CHECKS", "25")
	t.Setenv("WORKER_HEARTBEAT_PERIOD", "5s")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.DBProbePeriod != 45*time.Second {
		t.Errorf("db probe = %s, beklenen 45s", cfg.DBProbePeriod)
	}
	if cfg.SchedulerPeriod != 2*time.Second {
		t.Errorf("scheduler = %s, beklenen 2s", cfg.SchedulerPeriod)
	}
	if cfg.MaxConcurrentChecks != 25 {
		t.Errorf("eşzamanlı kontrol = %d, beklenen 25", cfg.MaxConcurrentChecks)
	}
	if cfg.Heartbeat.Period != 5*time.Second {
		t.Errorf("heartbeat = %s, beklenen 5s", cfg.Heartbeat.Period)
	}
}

// Varsayılanlar: scheduler turu ve eşzamanlılık sınırı ayar gerektirmeden
// çalışır durumda olmalı (klonlayan herkes `docker compose up` diyebilsin).
func TestLoadWorkerSchedulerDefaults(t *testing.T) {
	setValidEnv(t)

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.SchedulerPeriod != defaultSchedulerPeriod {
		t.Errorf("scheduler = %s, beklenen %s", cfg.SchedulerPeriod, defaultSchedulerPeriod)
	}
	if cfg.MaxConcurrentChecks != defaultMaxConcurrentChecks {
		t.Errorf("eşzamanlı kontrol = %d, beklenen %d", cfg.MaxConcurrentChecks, defaultMaxConcurrentChecks)
	}
	if cfg.RollupPeriod != defaultRollupPeriod {
		t.Errorf("rollup turu = %s, beklenen %s", cfg.RollupPeriod, defaultRollupPeriod)
	}
	if cfg.RetentionPeriod != defaultRetentionPeriod {
		t.Errorf("budama turu = %s, beklenen %s", cfg.RetentionPeriod, defaultRetentionPeriod)
	}
	if cfg.RawRetentionDays != defaultRawRetentionDays {
		t.Errorf("saklama süresi = %d gün, beklenen %d", cfg.RawRetentionDays, defaultRawRetentionDays)
	}
}

func TestLoadWorkerRejectsInvalidPeriods(t *testing.T) {
	cases := map[string][2]string{
		"bozuk db probe süresi":   {"WORKER_DB_PROBE_PERIOD", "beş saniye"},
		"sıfır db probe süresi":   {"WORKER_DB_PROBE_PERIOD", "0s"},
		"negatif db probe süresi": {"WORKER_DB_PROBE_PERIOD", "-1s"},
		"bozuk kalp atışı süresi": {"WORKER_HEARTBEAT_PERIOD", "beş saniye"},
		"sıfır kalp atışı süresi": {"WORKER_HEARTBEAT_PERIOD", "0s"},
		"negatif kapanma süresi":  {"SHUTDOWN_TIMEOUT", "-1s"},
		"sıfır scheduler turu":    {"WORKER_SCHEDULER_PERIOD", "0s"},
		"bozuk scheduler turu":    {"WORKER_SCHEDULER_PERIOD", "iki saniye"},
		// Eşzamanlılık sınırı: 0 anlamsız, üst sınır ise tek bir yazım
		// hatasının (10000) makineyi boğmasını engelliyor.
		"sıfır eşzamanlılık": {"WORKER_MAX_CONCURRENT_CHECKS", "0"},
		"aşırı eşzamanlılık": {"WORKER_MAX_CONCURRENT_CHECKS", "10000"},
		"sayı olmayan sınır": {"WORKER_MAX_CONCURRENT_CHECKS", "çok"},
		// Toplulaştırma/budama (3.4).
		"sıfır rollup turu":    {"WORKER_ROLLUP_PERIOD", "0s"},
		"bozuk rollup turu":    {"WORKER_ROLLUP_PERIOD", "beş dakika"},
		"sıfır budama turu":    {"WORKER_RETENTION_PERIOD", "0s"},
		"sıfır saklama süresi": {"RAW_RETENTION_DAYS", "0"},
		"aşırı saklama süresi": {"RAW_RETENTION_DAYS", "5000"},
		"sayı olmayan saklama": {"RAW_RETENTION_DAYS", "otuz"},
	}

	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(pair[0], pair[1])

			if _, err := LoadWorker(); err == nil {
				t.Fatalf("%s=%q kabul edildi, hata bekleniyordu", pair[0], pair[1])
			}
		})
	}
}

// Kalp atışı dosyasının varsayılanı geçici dizinde: klonlayan hiç kimse ayar
// yapmadan (ve konteynerde yazma izni aramadan) çalıştırabilsin.
func TestLoadWorkerHeartbeatDefaultsToTempDir(t *testing.T) {
	setValidEnv(t)

	heartbeat, err := LoadWorkerHeartbeat()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if heartbeat.Period != defaultHeartbeatPeriod {
		t.Errorf("periyot = %s, beklenen %s", heartbeat.Period, defaultHeartbeatPeriod)
	}

	if want := filepath.Join(os.TempDir(), heartbeatFileName); heartbeat.Path != want {
		t.Errorf("yol = %q, beklenen %q", heartbeat.Path, want)
	}
}

func TestLoadWorkerHeartbeatUsesExplicitPath(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WORKER_HEARTBEAT_FILE", filepath.Join("özel", "dizin", "beat"))

	heartbeat, err := LoadWorkerHeartbeat()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if want := filepath.Join("özel", "dizin", "beat"); heartbeat.Path != want {
		t.Errorf("yol = %q, beklenen %q", heartbeat.Path, want)
	}
}

// `worker healthcheck` yalnızca bu iki değeri okuyor: DATABASE_URL yoksa da
// çalışmalı (liveness'ın veritabanıyla ilgisi yok — 0.3'ün /health ayrımı).
func TestLoadWorkerHeartbeatIgnoresDatabaseURL(t *testing.T) {
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "")

	if _, err := LoadWorkerHeartbeat(); err != nil {
		t.Fatalf("DATABASE_URL olmadan hata: %v", err)
	}

	if _, err := LoadWorker(); err == nil {
		t.Fatal("worker DATABASE_URL olmadan yapılandırıldı")
	}
}

// --- APP_ENV -----------------------------------------------------------------

// Bilinmeyen bir APP_ENV sessizce development'a düşmemeli: "prod" yazan bir
// dağıtım, geliştirme davranışıyla (ör. otomatik anahtar üretimi) çalışırdı.
func TestUnknownAppEnvIsRejected(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "prod")

	if _, err := LoadAPI(); err == nil {
		t.Error("api: bilinmeyen APP_ENV kabul edildi")
	}
	if _, err := LoadWorker(); err == nil {
		t.Error("worker: bilinmeyen APP_ENV kabul edildi")
	}
	if _, err := LoadMigrate(); err == nil {
		t.Error("migrate: bilinmeyen APP_ENV kabul edildi")
	}
}

// --- İmzalama anahtarları ----------------------------------------------------

func TestValidSecretsAreUsedAsGiven(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "production")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if string(cfg.JWT.AccessSecret) != testAccessSecret {
		t.Error("verilen access anahtarı kullanılmadı")
	}
	if string(cfg.Refresh.Secret) != testRefreshSecret {
		t.Error("verilen refresh anahtarı kullanılmadı")
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("uyarı beklenmiyordu: %v", cfg.Warnings)
	}
	if cfg.JWT.AccessTTL != defaultAccessTTL {
		t.Errorf("accessTTL = %s, beklenen %s", cfg.JWT.AccessTTL, defaultAccessTTL)
	}
	if cfg.Refresh.TTL != defaultRefreshTTL {
		t.Errorf("refreshTTL = %s, beklenen %s", cfg.Refresh.TTL, defaultRefreshTTL)
	}
}

// .env.example'daki placeholder HİÇBİR ortamda anahtar olarak
// kullanılmamalı: bilinen bir imzalama anahtarı, herkesin herkes adına
// token üretebilmesi demek (canny-mini'de `changeme_*` sırları gerçek bir
// açıktı).
func TestPlaceholderSecretIsNeverUsed(t *testing.T) {
	placeholders := []string{"__GENERATE_ME__", "changeme_access_secret_changeme_1", "PLACEHOLDER-SECRET-PLACEHOLDER-01"}

	for _, placeholder := range placeholders {
		t.Run(placeholder, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("APP_ENV", "development")
			t.Setenv("JWT_ACCESS_SECRET", placeholder)

			cfg, err := LoadAPI()
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}

			if string(cfg.JWT.AccessSecret) == placeholder {
				t.Fatal("placeholder değer imzalama anahtarı olarak kullanıldı")
			}
		})
	}
}

// Geliştirme ortamında placeholder/eksik anahtar boot'u engellemiyor:
// klonlayan biri `cp .env.example .env && docker compose up` ile hiçbir şey
// üretmeden çalıştırabilmeli (docs/0.0-constraints.md §3). Bunun bedeli
// (yeniden başlatınca oturumların düşmesi) uyarı olarak bildiriliyor.
func TestDevelopmentGeneratesEphemeralSecrets(t *testing.T) {
	for _, env := range []string{"development", "test"} {
		t.Run(env, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("APP_ENV", env)
			t.Setenv("JWT_ACCESS_SECRET", "__GENERATE_ME__")
			t.Setenv("REFRESH_TOKEN_SECRET", "__GENERATE_ME__")

			cfg, err := LoadAPI()
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}

			if len(cfg.JWT.AccessSecret) < minSecretLength {
				t.Errorf("üretilen anahtar çok kısa: %d", len(cfg.JWT.AccessSecret))
			}
			if string(cfg.JWT.AccessSecret) == string(cfg.Refresh.Secret) {
				t.Error("üretilen iki anahtar aynı")
			}
			if len(cfg.Warnings) != 2 {
				t.Fatalf("uyarı sayısı = %d, beklenen 2 (%v)", len(cfg.Warnings), cfg.Warnings)
			}
			for _, warning := range cfg.Warnings {
				if !strings.Contains(warning, "geçici imzalama anahtarı üretildi") ||
					!strings.Contains(warning, "openssl rand -hex 32") {
					t.Errorf("uyarı metni yetersiz: %q", warning)
				}
			}
		})
	}
}

// İki ayrı çağrı iki ayrı anahtar üretmeli — "rastgele" gerçekten rastgele
// olsun.
func TestGeneratedSecretsDifferBetweenBoots(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("JWT_ACCESS_SECRET", "__GENERATE_ME__")

	first, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	second, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if string(first.JWT.AccessSecret) == string(second.JWT.AccessSecret) {
		t.Error("iki boot aynı anahtarı üretti")
	}
}

func TestProductionRefusesToBootWithoutRealSecret(t *testing.T) {
	cases := map[string]string{
		"placeholder": "__GENERATE_ME__",
		"boş":         "",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("APP_ENV", "production")
			t.Setenv("JWT_ACCESS_SECRET", value)

			if _, err := LoadAPI(); err == nil {
				t.Fatal("production'da geçersiz anahtarla boot edildi")
			}
		})
	}
}

// APP_ENV set edilmemişse otomatik üretim YAPILMAZ: varsayılanın
// development olması, APP_ENV'i geçmeyi unutan bir üretim dağıtımının
// sessizce rastgele anahtarla çalışmasına yol açmamalı.
func TestMissingAppEnvDoesNotGenerateSecrets(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "")
	t.Setenv("JWT_ACCESS_SECRET", "__GENERATE_ME__")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("APP_ENV verilmeden anahtar üretildi")
	}
}

// Güvenilen kaynak listesi VARSAYILAN OLARAK BOŞ: yapılandırma yapılmadıkça
// hiçbir `X-Forwarded-For` okunmuyor (0.4'ün `middleware.RealIP` kararı).
func TestTrustedProxiesDefaultsToEmpty(t *testing.T) {
	setValidEnv(t)

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, varsayılan boş olmalı", cfg.TrustedProxies)
	}
}

func TestTrustedProxiesAreParsedAsList(t *testing.T) {
	setValidEnv(t)
	t.Setenv("TRUSTED_PROXIES", " 10.9.0.1 , 172.18.0.0/16 ")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if len(cfg.TrustedProxies) != 2 ||
		cfg.TrustedProxies[0] != "10.9.0.1" || cfg.TrustedProxies[1] != "172.18.0.0/16" {
		t.Errorf("TrustedProxies = %#v", cfg.TrustedProxies)
	}
}

// SSRF GEVŞETME KAPISI, imzalama anahtarı üretimiyle AYNI disipline tabi
// (7.4'ün yazdığı gibi): APP_ENV **açıkça** development veya test olmalı.
//
// Varsayılanın development olması yeterli sayılsaydı, APP_ENV'i geçmeyi
// unutan bir dağıtımda tek bir ortam değişkeni (CHECKER_ALLOW_PRIVATE_TARGETS)
// korumayı tamamen kapatırdı — 1.1'in anahtar üretiminde kapatılan kapının
// aynısı burada açık kalırdı.
func TestPrivateTargetGateNeedsExplicitEnv(t *testing.T) {
	for _, tc := range []struct {
		appEnv string
		allow  bool
	}{
		{appEnv: "", allow: false},           // APP_ENV verilmedi → kapı KAPALI
		{appEnv: "development", allow: true}, // açıkça development → açık
		{appEnv: "test", allow: true},        // açıkça test → açık
		{appEnv: "production", allow: false}, // üretim → etkisiz
	} {
		t.Run("APP_ENV="+tc.appEnv, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("APP_ENV", tc.appEnv)
			t.Setenv("CHECKER_ALLOW_PRIVATE_TARGETS", "true")

			cfg, err := LoadAPI()
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}

			if cfg.DialPolicy.AllowPrivate != tc.allow {
				t.Errorf("AllowPrivate = %v, beklenen %v", cfg.DialPolicy.AllowPrivate, tc.allow)
			}

			// Kapı isteğe rağmen açılmadıysa bu SESSİZ kalmamalı: yapılandırmayı
			// yazan kişi neden işe yaramadığını görebilmeli.
			if !tc.allow && len(cfg.Warnings) == 0 {
				t.Error("kapı yok sayıldı ama uyarı üretilmedi")
			}
		})
	}
}

// worker de kontrolleri atan taraf: aynı kapı, aynı kural.
func TestPrivateTargetGateNeedsExplicitEnvForWorker(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "")
	t.Setenv("CHECKER_ALLOW_PRIVATE_TARGETS", "true")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.DialPolicy.AllowPrivate {
		t.Error("APP_ENV verilmeden worker'da SSRF koruması gevşetildi")
	}
}

// Tanınmayan bir değer sessizce "false" olmamalı: bu bir güvenlik kapısı ve
// sessiz varsayılan, kapının hangi tarafta olduğunu belirsiz bırakırdı.
func TestPrivateTargetGateRejectsUnknownValue(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("CHECKER_ALLOW_PRIVATE_TARGETS", "yes please")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("tanınmayan değer kabul edildi")
	}
}

func TestShortSecretIsRejectedEverywhere(t *testing.T) {
	for _, env := range []string{"development", "test", "production"} {
		t.Run(env, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("APP_ENV", env)
			t.Setenv("JWT_ACCESS_SECRET", "kisa-anahtar")

			if _, err := LoadAPI(); err == nil {
				t.Fatal("32 karakterden kısa anahtar kabul edildi")
			}
		})
	}
}

// Access ve refresh aynı anahtarla imzalanırsa, iki token türünü ayıran tek
// şey gövdedeki tür alanı kalır. Tek savunmaya güvenilmiyor.
func TestIdenticalAccessAndRefreshSecretsAreRejected(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("REFRESH_TOKEN_SECRET", testAccessSecret)

	if _, err := LoadAPI(); err == nil {
		t.Fatal("aynı access ve refresh anahtarı kabul edildi")
	}
}

// Refresh token'ın access token'dan kısa yaşaması, uzun oturum fikrini
// anlamsız kılardı.
func TestRefreshTTLIsValidated(t *testing.T) {
	cases := map[string]struct{ access, refresh string }{
		"sıfır":           {"", "0s"},
		"negatif":         {"", "-1h"},
		"üst sınır üstü":  {"", "9000h"},
		"sayı değil":      {"", "otuz gün"},
		"access'ten kısa": {"30m", "10m"},
		"access ile eşit": {"30m", "30m"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("JWT_ACCESS_TTL", tc.access)
			t.Setenv("REFRESH_TOKEN_TTL", tc.refresh)

			if _, err := LoadAPI(); err == nil {
				t.Fatalf("geçersiz REFRESH_TOKEN_TTL kabul edildi: %q", tc.refresh)
			}
		})
	}
}

func TestRefreshTTLCanBeOverridden(t *testing.T) {
	setValidEnv(t)
	t.Setenv("REFRESH_TOKEN_TTL", "168h")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.Refresh.TTL != 7*24*time.Hour {
		t.Errorf("refreshTTL = %s, beklenen 168h", cfg.Refresh.TTL)
	}
}

func TestAccessTTLIsValidated(t *testing.T) {
	cases := map[string]string{
		"sıfır":          "0s",
		"negatif":        "-5m",
		"üst sınır üstü": "2h",
		"sayı değil":     "onbeş dakika",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("JWT_ACCESS_TTL", value)

			if _, err := LoadAPI(); err == nil {
				t.Fatalf("geçersiz JWT_ACCESS_TTL kabul edildi: %q", value)
			}
		})
	}
}

func TestAccessTTLCanBeOverridden(t *testing.T) {
	setValidEnv(t)
	t.Setenv("JWT_ACCESS_TTL", "5m")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if cfg.JWT.AccessTTL != 5*time.Minute {
		t.Errorf("accessTTL = %s, beklenen 5m", cfg.JWT.AccessTTL)
	}
}

// GELİŞTİRİCİ POSTA KUTUSU KAPISI DA APP_ENV'İN AÇIKÇA VERİLMESİNİ İSTİYOR
// (8.2) — 1.1'in imzalama anahtarı ve 7.4/7.5'in SSRF kapısıyla AYNI kural.
//
// Kritik satır ilk vaka: `environmentFromEnv` APP_ENV verilmediğinde
// `development` döndürüyor, yani yalnızca çözülmüş ortama bakan bir kapı,
// APP_ENV geçmeyi unutan bir dağıtımda posta kutusunu KENDİLİĞİNDEN açardı.
// Posta kutusu onay/davet/parola sıfırlama linklerini düz metin taşıyor.
func TestDevToolsGateNeedsExplicitEnv(t *testing.T) {
	for _, tc := range []struct {
		appEnv  string
		enabled bool
	}{
		{appEnv: "", enabled: false},           // APP_ENV verilmedi → KAPALI
		{appEnv: "development", enabled: true}, // açıkça development → açık
		{appEnv: "test", enabled: true},        // açıkça test → açık
		{appEnv: "production", enabled: false}, // üretim → kapalı
	} {
		t.Run("APP_ENV="+tc.appEnv, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("APP_ENV", tc.appEnv)

			apiCfg, err := LoadAPI()
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}
			if apiCfg.DevTools != tc.enabled {
				t.Errorf("api DevTools = %v, beklenen %v", apiCfg.DevTools, tc.enabled)
			}

			// worker'da da AYNI cevap çıkmalı: iki süreç ayrışsaydı posta
			// kutusu kesinti bildirimlerini gösterir, abonelik onaylarını
			// göstermezdi (ya da tersi).
			workerCfg, err := LoadWorker()
			if err != nil {
				t.Fatalf("beklenmeyen hata (worker): %v", err)
			}
			if workerCfg.DevTools != tc.enabled {
				t.Errorf("worker DevTools = %v, beklenen %v", workerCfg.DevTools, tc.enabled)
			}
		})
	}
}
