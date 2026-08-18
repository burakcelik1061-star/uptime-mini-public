package testdb

import "testing"

// Bu paketin en yıkıcı hareketi `TRUNCATE`; koruması da bu yüzden bir
// yorum satırı değil, testi olan bir fonksiyon (7.1).
func TestGuardTestDatabaseAcceptsTestNames(t *testing.T) {
	names := []string{
		"uptime_mini_test",
		"uptime_mini_test_orgs",
		"uptime_mini_test_api",
		"uptime_mini_test_status_page",
	}

	for _, name := range names {
		if err := guardTestDatabase(name); err != nil {
			t.Errorf("guardTestDatabase(%q) reddetti: %v", name, err)
		}
	}
}

// Reddedilenlerin hepsi gerçek bir kaza senaryosu: `.env`'deki geliştirme
// veritabanına doğrudan bağlanmak, üretim adı, ya da "içinde test geçiyor"
// diye onaylanan bir geliştirme veritabanı.
func TestGuardTestDatabaseRefusesEverythingElse(t *testing.T) {
	names := []string{
		"uptime_mini",
		"postgres",
		"uptime_mini_prod",
		"testbed",          // "test" içeriyor ama `_test` ile bitmiyor
		"uptime_mini_TEST", // desen küçük harf; büyük harfli ad onaylanmıyor
		"uptime_mini_test2",
	}

	for _, name := range names {
		if err := guardTestDatabase(name); err == nil {
			t.Errorf("guardTestDatabase(%q) kabul etti — bu ad geliştirme verisi taşıyabilir", name)
		}
	}
}

func TestDatabaseNameReadsConnectionString(t *testing.T) {
	name, err := databaseName("postgres://u:p@localhost:5433/uptime_mini_test_orgs?sslmode=disable")
	if err != nil {
		t.Fatalf("databaseName: %v", err)
	}
	if name != "uptime_mini_test_orgs" {
		t.Errorf("ad = %q, beklenen uptime_mini_test_orgs", name)
	}

	if _, err := databaseName("postgres://u:p@localhost:5433"); err == nil {
		t.Error("veritabanı adı olmayan dizeye hata verilmedi")
	}
}
