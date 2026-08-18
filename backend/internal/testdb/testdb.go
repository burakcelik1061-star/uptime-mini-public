// Package testdb, veritabanı gerektiren testlerin ortak kurulumudur.
//
// Yalnızca `_test.go` dosyalarından kullanılıyor, dolayısıyla üretim
// binary'lerine girmiyor. Ayrı bir paket olmasının sebebi, aynı kurulumun
// (test veritabanını oluştur, migration'ları uygula, tabloları temizle)
// birden fazla pakette tekrarlanması gereği — internal/sessions bunu 1.2'de
// kendi içinde tutuyordu, internal/orgs gelince ortaklaştırıldı.
//
// Testler GELİŞTİRME veritabanında değil, adının sonuna `_test` eklenmiş
// AYRI bir veritabanında koşuyor: `go test ./...` çalıştırmak kimsenin
// local verisini silmemeli. Bu kural 7.1'de bir TEMENNİ olmaktan çıkarıldı:
// adı test desenine uymayan bir veritabanına bağlanıldığında paket
// çalışmayı reddediyor (bkz. guardTestDatabase).
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // goose için database/sql sürücüsü
	"github.com/pressly/goose/v3"

	"github.com/burakcelik1061-star/uptime-mini/backend/db"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/config"
)

// prepared, veritabanı adı başına tek kurulum (oluştur + migrate).
var prepared sync.Map

// testDatabaseName, bu paketin dokunmasına izin verilen veritabanı adlarını
// tanımlar: ad `_test` ile ya da `_test_<paket>` ile BİTMEK zorunda.
//
// canny-mini'de bu koruma geliştirme verisini kurtarmıştı: yanlış bir
// DATABASE_URL ile koşan bir test turu, tabloları `TRUNCATE` ederek local
// veriyi siler. Desen bilinçli olarak DAR: "içinde test geçsin" yeterli
// olsaydı `testbed` adlı bir geliştirme veritabanı da onaylanırdı.
var testDatabaseName = regexp.MustCompile(`_test(_[a-z0-9_]+)?$`)

// guardTestDatabase, adı test desenine uymayan veritabanında çalışmayı
// reddeder. Hata metni ne yapılması gerektiğini de söylüyor.
func guardTestDatabase(name string) error {
	if testDatabaseName.MatchString(name) {
		return nil
	}

	return fmt.Errorf(
		"testler %q veritabanında koşmayı reddetti: ad `_test` ile bitmiyor. "+
			"Testler yalnızca adı `_test`/`_test_<paket>` ile biten AYRI bir "+
			"veritabanında koşar — aksi hâlde bu tur geliştirme verisini silerdi",
		name)
}

// databaseName, bağlantı dizesindeki veritabanı adını döner.
func databaseName(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" {
		return "", fmt.Errorf("bağlantı dizesi bir veritabanı adı içermiyor")
	}

	return name, nil
}

// URL, test veritabanının bağlantı dizesini döner; ilk çağrıda veritabanını
// oluşturup migration'ları uygular.
//
// `owner` ÇAĞIRAN PAKETİN adı ve veritabanı adına ekleniyor
// (`uptime_mini_test_orgs` gibi). Sebebi yaşanarak öğrenildi: `go test`
// paketleri PARALEL süreçlerde koşturuyor; tek bir test veritabanı
// paylaşıldığında paketler birbirinin tablolarını ortasında `TRUNCATE`
// ediyor, hatta migration'ları aynı anda uygulamaya çalışıp
// "duplicate key ... pg_type" hatası veriyordu.
//
// BİRİM ↔ INTEGRATION AYRIMI BURADA (7.1, kullanıcı kararı): `go test -short`
// bu paketi çağıran her testi atlar, yani "integration testi" bu projede
// "gerçek veritabanına dokunan test" demek ve ayrım TEK YERDE yazılı.
// Build tag'i seçilmedi: tag'i unutmak testleri SESSİZCE hiç koşturmamak
// olurdu ve varsayılan yön güvensiz hâle gelirdi. Burada varsayılan
// (`go test ./...`) HER ŞEYİ koşuyor, `-short` bilinçli bir vazgeçiş.
//
// DATABASE_URL tanımlı değilse test ATLANIR: veritabanı olmadan da
// `go test ./...` yeşil kalabilsin. CI'DA ATLAMA YOK: orada değişkenin
// yokluğu hata: kritik iş mantığının testi "yeşil göründü ama hiç koşmadı"
// hâline düşemesin.
func URL(t *testing.T, owner string) string {
	t.Helper()

	if testing.Short() {
		t.Skip("-short: veritabanı gerektiren testler atlanıyor")
	}

	// Yerelde repo kökündeki `.env`'den, CI'da ortamdan.
	config.LoadDotEnv()

	base := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if base == "" {
		// `CI` GitHub Actions'ın kendi değişkeni; yerelde tanımlı değil.
		if os.Getenv("CI") != "" {
			t.Fatal("CI'da DATABASE_URL tanımlı değil — veritabanı testleri " +
				"sessizce atlanamaz (bkz. .github/workflows/ci.yml)")
		}
		t.Skip("DATABASE_URL tanımlı değil — veritabanı gerektiren testler atlanıyor")
	}

	testURL, err := withDatabaseSuffix(base, "_test_"+owner)
	if err != nil {
		t.Fatalf("test veritabanı adresi üretilemedi: %v", err)
	}

	name, err := databaseName(testURL)
	if err != nil {
		t.Fatalf("test veritabanı adı okunamadı: %v", err)
	}
	if err := guardTestDatabase(name); err != nil {
		t.Fatal(err)
	}

	once, _ := prepared.LoadOrStore(testURL, &sync.Once{})
	once.(*sync.Once).Do(func() {
		if err := createDatabase(base, testURL); err != nil {
			t.Fatalf("test veritabanı oluşturulamadı: %v", err)
		}
		if err := migrate(testURL); err != nil {
			t.Fatalf("migration uygulanamadı: %v", err)
		}
	})

	return testURL
}

// Pool, tabloları temizlenmiş bir veritabanına bağlantı havuzu döner.
// Havuz test bitiminde kapatılıyor. `owner` için bkz. URL.
func Pool(t *testing.T, owner string) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(t.Context(), URL(t, owner))
	if err != nil {
		t.Fatalf("havuz kurulamadı: %v", err)
	}
	t.Cleanup(pool.Close)

	truncateAll(t, pool)

	return pool
}

// truncateAll, şemadaki tüm tabloları boşaltır.
//
// Tablo adları elle sayılmıyor: her yeni migration'da bu listeyi güncellemek
// unutulur ve testler birbirinin verisini görmeye başlar. goose'un kendi
// sürüm tablosu korunuyor — silinseydi migration'lar her testte yeniden
// uygulanmaya çalışılırdı.
func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	// KORUMA BURADA DA TEKRARLANIYOR ve bu kez SUNUCUYA soruluyor: yıkıcı
	// hareket `TRUNCATE`, ve hangi veritabanına yazdığımızın tek doğru
	// cevabı bağlantının kendisinde. Bağlantı dizesinden okunan ad
	// yanıltabilir (`PGDATABASE`, servis dosyası, connection string'deki
	// ikinci bir `dbname` parametresi hepsi hedefi değiştirir).
	var connected string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&connected); err != nil {
		t.Fatalf("bağlı olunan veritabanı okunamadı: %v", err)
	}
	if err := guardTestDatabase(connected); err != nil {
		t.Fatal(err)
	}

	// PARTITION'LAR LİSTEYE GİRMİYOR: bölümlenmiş bir tabloyu (2.3'ün
	// `check_results`'ı) TRUNCATE etmek zaten tüm partition'larını boşaltıyor.
	// Hepsini tek tek saymak her testte ~90 fazladan tablo adı demekti ve
	// test süresini dört katına çıkarmıştı.
	var tables *string
	row := pool.QueryRow(t.Context(), `
		SELECT string_agg(quote_ident(c.relname), ', ')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind IN ('r', 'p')
		  AND NOT c.relispartition
		  AND c.relname <> 'goose_db_version'`)
	if err := row.Scan(&tables); err != nil {
		t.Fatalf("tablolar listelenemedi: %v", err)
	}

	if tables == nil {
		return
	}

	if _, err := pool.Exec(t.Context(), "TRUNCATE "+*tables+" RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("tablolar temizlenemedi: %v", err)
	}
}

func withDatabaseSuffix(raw, suffix string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" {
		return "", fmt.Errorf("DATABASE_URL bir veritabanı adı içermiyor")
	}

	parsed.Path = "/" + name + suffix

	return parsed.String(), nil
}

// createDatabase, test veritabanını yoksa oluşturur.
func createDatabase(baseURL, testURL string) error {
	parsed, err := url.Parse(testURL)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(parsed.Path, "/")

	pool, err := pgxpool.New(context.Background(), baseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// CREATE DATABASE parametre almıyor; ad ortam değişkeninden geldiği için
	// tanımlayıcı olarak tırnaklanıyor.
	quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`

	// "already exists" (42P04) beklenen bir sonuç.
	if _, err := pool.Exec(context.Background(), "CREATE DATABASE "+quoted); err != nil &&
		!strings.Contains(err.Error(), "42P04") &&
		!strings.Contains(err.Error(), "already exists") {
		return err
	}

	return nil
}

// migrate, gömülü goose migration'larını test veritabanına uygular.
func migrate(testURL string) error {
	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(goose.NopLogger())

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	sqlDB, err := sql.Open("pgx", testURL)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	return goose.UpContext(context.Background(), sqlDB, db.MigrationsDir)
}
