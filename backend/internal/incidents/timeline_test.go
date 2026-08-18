package incidents

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// createUser, timeline güncellemelerinin yazarı.
func createUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()

	user, err := store.New(pool).CreateUser(t.Context(), store.CreateUserParams{
		Email:        "yazar-" + uuid.NewString() + "@example.com",
		PasswordHash: "$argon2id$test",
		Name:         "Nöbetçi",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	return user.ID
}

// openAutoIncident, testler için bir kesinti açar (4.1 yolundan).
func openAutoIncident(t *testing.T, pool *pgxpool.Pool, monitor store.Monitor, at time.Time) store.Incident {
	t.Helper()

	svc, _ := newService(t, pool)

	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)
	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at); err != nil {
		t.Fatalf("incident açılamadı: %v", err)
	}

	return openIncidentFor(t, pool, monitor.ID)
}

// OTOMATİK AÇMA/KAPAMA TIMELINE'A DA YAZIYOR (4.2): "bu incident'ı ne açtı"
// sorusunun cevabı geçmişin içinde, yalnızca worker logunda değil.
func TestAutoIncidentWritesSystemUpdates(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	base := time.Now().UTC().Add(-10 * time.Minute)
	incident := openAutoIncident(t, pool, monitor, base)

	detail, err := svc.Get(t.Context(), orgID, incident.ID)
	if err != nil {
		t.Fatalf("incident okunamadı: %v", err)
	}

	if len(detail.Updates) != 1 {
		t.Fatalf("güncelleme sayısı = %d, beklenen 1", len(detail.Updates))
	}

	opened := detail.Updates[0]
	if !opened.IsSystem {
		t.Error("açılış satırı is_system=false geldi")
	}
	if opened.AuthorID != nil {
		t.Errorf("sistem satırının yazarı = %v, beklenen NULL", *opened.AuthorID)
	}
	if opened.Status != store.IncidentStatusInvestigating {
		t.Errorf("açılış satırının durumu = %q, beklenen investigating", opened.Status)
	}

	recovered := base.Add(2 * time.Minute)
	recordCheck(t, pool, monitor.ID, recovered, store.CheckOutcomeSuccess)
	if _, err := svc.ApplyTransition(t.Context(), monitor, upTransition(1), recovered); err != nil {
		t.Fatalf("incident kapatılamadı: %v", err)
	}

	detail, err = svc.Get(t.Context(), orgID, incident.ID)
	if err != nil {
		t.Fatalf("incident okunamadı: %v", err)
	}

	if len(detail.Updates) != 2 {
		t.Fatalf("güncelleme sayısı = %d, beklenen 2", len(detail.Updates))
	}

	closed := detail.Updates[1]
	if !closed.IsSystem || closed.Status != store.IncidentStatusResolved {
		t.Errorf("kapanış satırı = %+v, beklenen sistem + resolved", closed)
	}

	// Timeline KRONOLOJİK: bir kesintinin anlatısı yukarıdan aşağıya okunur.
	if closed.CreatedAt.Before(opened.CreatedAt) {
		t.Error("güncellemeler kronolojik sırada değil")
	}
}

// Güncelleme eklemek incident'ın DURUMUNU da taşıyor — tek ifadede
// (timeline'da "çözüldü" yazarken incident'ın açık kalması imkânsız).
func TestAddUpdateMovesIncidentStatus(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	incident := openAutoIncident(t, pool, monitor, time.Now().UTC().Add(-5*time.Minute))
	author := createUser(t, pool)

	update, err := svc.AddUpdate(t.Context(), orgID, incident.ID, UpdateInput{
		AuthorID: author,
		Status:   store.IncidentStatusIdentified,
		Body:     "  Veritabanı bağlantı havuzu doldu.  ",
	})
	if err != nil {
		t.Fatalf("güncelleme eklenemedi: %v", err)
	}

	if update.IsSystem {
		t.Error("kullanıcı satırı is_system=true geldi")
	}
	if update.AuthorID == nil || *update.AuthorID != author {
		t.Errorf("yazar = %v, beklenen %v", update.AuthorID, author)
	}
	if update.Body != "Veritabanı bağlantı havuzu doldu." {
		t.Errorf("gövde kırpılmadı: %q", update.Body)
	}

	detail, err := svc.Get(t.Context(), orgID, incident.ID)
	if err != nil {
		t.Fatalf("incident okunamadı: %v", err)
	}

	if detail.Incident.Status != store.IncidentStatusIdentified {
		t.Errorf("incident durumu = %q, beklenen identified", detail.Incident.Status)
	}
	if detail.Incident.ResolvedAt != nil {
		t.Error("incident kapanmış görünüyor")
	}
	if len(detail.Updates) != 2 {
		t.Fatalf("güncelleme sayısı = %d, beklenen 2 (sistem + kullanıcı)", len(detail.Updates))
	}
}

// `resolved` güncellemesi incident'ı KAPATIYOR ve kapanmış incident'a bir daha
// yazılamıyor (yeniden açma bilinçli olarak yok).
func TestAddUpdateResolvesAndLocksIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	incident := openAutoIncident(t, pool, monitor, time.Now().UTC().Add(-5*time.Minute))
	author := createUser(t, pool)

	if _, err := svc.AddUpdate(t.Context(), orgID, incident.ID, UpdateInput{
		AuthorID: author,
		Status:   store.IncidentStatusResolved,
		Body:     "Elle kapatıldı: servis normale döndü.",
	}); err != nil {
		t.Fatalf("kapatma güncellemesi eklenemedi: %v", err)
	}

	detail, err := svc.Get(t.Context(), orgID, incident.ID)
	if err != nil {
		t.Fatalf("incident okunamadı: %v", err)
	}

	if detail.Incident.Status != store.IncidentStatusResolved || detail.Incident.ResolvedAt == nil {
		t.Fatalf("incident kapanmadı: %+v", detail.Incident)
	}

	_, err = svc.AddUpdate(t.Context(), orgID, incident.ID, UpdateInput{
		AuthorID: author,
		Status:   store.IncidentStatusMonitoring,
		Body:     "Yeniden açmayı deniyorum.",
	})
	if !errors.Is(err, ErrResolved) {
		t.Fatalf("hata = %v, beklenen ErrResolved", err)
	}
}

// ELLE KAPATILAN INCIDENT'I WORKER YENİDEN AÇMIYOR: monitor hâlâ `down` ve
// bir sonraki geçiş kurtarma olacak; kapanmış incident'ı yeniden kapatmaya
// çalışmak da hata değil.
func TestManuallyResolvedIncidentIsNotReopenedByRecovery(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	base := time.Now().UTC().Add(-5 * time.Minute)
	incident := openAutoIncident(t, pool, monitor, base)

	if _, err := svc.AddUpdate(t.Context(), orgID, incident.ID, UpdateInput{
		AuthorID: createUser(t, pool),
		Status:   store.IncidentStatusResolved,
		Body:     "Elle kapatıldı.",
	}); err != nil {
		t.Fatalf("kapatma güncellemesi eklenemedi: %v", err)
	}

	recovered := base.Add(time.Minute)
	recordCheck(t, pool, monitor.ID, recovered, store.CheckOutcomeSuccess)

	action, err := svc.ApplyTransition(t.Context(), monitor, upTransition(1), recovered)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionNoOpenIncident {
		t.Fatalf("action = %q, beklenen %q", action, ActionNoOpenIncident)
	}

	if got := countIncidents(t, pool, monitor.ID); got != 1 {
		t.Fatalf("incident sayısı = %d, beklenen 1", got)
	}
}

// Süzgeç: açık / kapanmış / hepsi.
func TestListFiltersByState(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	first := createMonitor(t, pool, orgID)
	second := createMonitor(t, pool, orgID)

	base := time.Now().UTC().Add(-30 * time.Minute)

	// Biri kapanmış, biri açık.
	closed := openAutoIncident(t, pool, first, base)
	if _, err := svc.AddUpdate(t.Context(), orgID, closed.ID, UpdateInput{
		AuthorID: createUser(t, pool),
		Status:   store.IncidentStatusResolved,
		Body:     "Kapandı.",
	}); err != nil {
		t.Fatalf("kapatılamadı: %v", err)
	}

	open := openAutoIncident(t, pool, second, base.Add(10*time.Minute))

	cases := []struct {
		filter Filter
		want   []uuid.UUID
	}{
		{filter: FilterOpen, want: []uuid.UUID{open.ID}},
		{filter: FilterResolved, want: []uuid.UUID{closed.ID}},
		// Yeniden eskiye: açık olan daha yeni.
		{filter: FilterAll, want: []uuid.UUID{open.ID, closed.ID}},
	}

	for _, tc := range cases {
		t.Run(string(tc.filter), func(t *testing.T) {
			items, err := svc.List(t.Context(), orgID, tc.filter, 0)
			if err != nil {
				t.Fatalf("liste okunamadı: %v", err)
			}

			if len(items) != len(tc.want) {
				t.Fatalf("satır sayısı = %d, beklenen %d", len(items), len(tc.want))
			}

			for i, want := range tc.want {
				if items[i].Incident.ID != want {
					t.Errorf("%d. satır = %v, beklenen %v", i, items[i].Incident.ID, want)
				}
			}
		})
	}
}

// Liste etkilenen servisleri de getiriyor ve monitor ADI canlı kayıttan
// okunuyor (incident satırına kopyalanmıyor).
func TestListIncludesAffectedMonitorsWithLiveNames(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	openAutoIncident(t, pool, monitor, time.Now().UTC().Add(-time.Minute))

	if _, err := pool.Exec(t.Context(),
		`UPDATE monitors SET name = 'Yeni Ad' WHERE id = $1`, monitor.ID); err != nil {
		t.Fatalf("monitor adı değiştirilemedi: %v", err)
	}

	items, err := svc.List(t.Context(), orgID, FilterAll, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}

	if len(items) != 1 || len(items[0].Monitors) != 1 {
		t.Fatalf("beklenen tek incident + tek servis, gelen: %+v", items)
	}
	if items[0].Monitors[0].Name != "Yeni Ad" {
		t.Errorf("servis adı = %q, beklenen \"Yeni Ad\"", items[0].Monitors[0].Name)
	}
	// Başlık ise açıldığı ANDAKİ adı taşıyor: geçmiş anlatı değişmemeli.
	if items[0].Incident.Title != AutoTitle(monitor.Name) {
		t.Errorf("başlık = %q, beklenen %q", items[0].Incident.Title, AutoTitle(monitor.Name))
	}
}

// ORG İZOLASYONU SQL'DE (2.1'in kuralı): başka organizasyonun incident'ı
// listede yok, tek satırlık okuma da "yok" diyor.
func TestIncidentsAreIsolatedByOrg(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	ownerOrg := createOrg(t, pool)
	otherOrg := createOrg(t, pool)

	monitor := createMonitor(t, pool, ownerOrg)
	incident := openAutoIncident(t, pool, monitor, time.Now().UTC().Add(-time.Minute))

	items, err := svc.List(t.Context(), otherOrg, FilterAll, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("yabancı organizasyon %d incident gördü", len(items))
	}

	if _, err := svc.Get(t.Context(), otherOrg, incident.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}

	if _, err := svc.AddUpdate(t.Context(), otherOrg, incident.ID, UpdateInput{
		AuthorID: createUser(t, pool),
		Status:   store.IncidentStatusMonitoring,
		Body:     "Başkasının kesintisine yazıyorum.",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}

	// Yabancı yazma denemesi timeline'ı da kirletmemiş olmalı.
	detail, err := svc.Get(t.Context(), ownerOrg, incident.ID)
	if err != nil {
		t.Fatalf("incident okunamadı: %v", err)
	}
	if len(detail.Updates) != 1 {
		t.Fatalf("güncelleme sayısı = %d, beklenen 1 (yalnızca sistem satırı)", len(detail.Updates))
	}
}

func TestGetUnknownIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	if _, err := svc.Get(t.Context(), createOrg(t, pool), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}
}

func TestListClampsLimit(t *testing.T) {
	cases := map[int32]int32{0: DefaultListLimit, -5: DefaultListLimit, 10: 10, 5000: MaxListLimit}

	for in, want := range cases {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %d, beklenen %d", in, got, want)
		}
	}
}

func TestValidateUpdate(t *testing.T) {
	valid := UpdateInput{Status: store.IncidentStatusMonitoring, Body: "izliyoruz"}

	if err := ValidateUpdate(valid); err != nil {
		t.Fatalf("geçerli girdi reddedildi: %v", err)
	}

	cases := []struct {
		name  string
		in    UpdateInput
		field string
	}{
		{
			name:  "boş gövde",
			in:    UpdateInput{Status: store.IncidentStatusMonitoring},
			field: "body",
		},
		{
			name:  "çok uzun gövde",
			in:    UpdateInput{Status: store.IncidentStatusMonitoring, Body: longBody()},
			field: "body",
		},
		{
			name:  "tanınmayan durum",
			in:    UpdateInput{Status: store.IncidentStatus("bilinmiyor"), Body: "x"},
			field: "status",
		},
		{
			// Boş dize sessizce "investigating"e düşmüyor.
			name:  "durum yok",
			in:    UpdateInput{Body: "x"},
			field: "status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var validation *ValidationError
			if err := ValidateUpdate(tc.in); !errors.As(err, &validation) {
				t.Fatalf("hata = %v, beklenen ValidationError", err)
			}

			if validation.Problems[0].Field != tc.field {
				t.Errorf("alan = %q, beklenen %q", validation.Problems[0].Field, tc.field)
			}
		})
	}
}

// Şema son hakem: 2000 karakteri aşan gövde doğrulama atlansa bile giremiyor.
func TestSchemaRejectsOversizedUpdateBody(t *testing.T) {
	pool := testdb.Pool(t, "incidents")

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	incident := openAutoIncident(t, pool, monitor, time.Now().UTC().Add(-time.Minute))

	_, err := pool.Exec(t.Context(),
		`INSERT INTO incident_updates (incident_id, status, body) VALUES ($1, 'monitoring', $2)`,
		incident.ID, longBody())
	if err == nil {
		t.Fatal("uzun gövde kabul edildi, beklenen: kısıt ihlali")
	}
}

// Sistem satırının yazarı olamaz (şemadaki CHECK).
func TestSchemaRejectsSystemUpdateWithAuthor(t *testing.T) {
	pool := testdb.Pool(t, "incidents")

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	incident := openAutoIncident(t, pool, monitor, time.Now().UTC().Add(-time.Minute))

	_, err := pool.Exec(t.Context(),
		`INSERT INTO incident_updates (incident_id, author_id, is_system, status, body)
		 VALUES ($1, $2, true, 'monitoring', 'x')`,
		incident.ID, createUser(t, pool))
	if err == nil {
		t.Fatal("yazarlı sistem satırı kabul edildi, beklenen: kısıt ihlali")
	}
}

func longBody() string {
	body := make([]rune, MaxUpdateBodyLength+1)
	for i := range body {
		body[i] = 'a'
	}

	return string(body)
}

// --- Manuel incident (4.4) ---------------------------------------------------

func manualInput(author uuid.UUID, monitorIDs ...uuid.UUID) ManualInput {
	return ManualInput{
		Title:      "Veritabanı bakımı kesintiye yol açtı",
		Impact:     store.IncidentImpactCritical,
		MonitorIDs: monitorIDs,
		AuthorID:   author,
		Body:       "  Sorunu araştırıyoruz.  ",
	}
}

// MANUEL INCIDENT BİRDEN FAZLA SERVİSİ KAPSAYABİLİYOR (0.1 §5) ve ilk timeline
// satırı kayıtla birlikte doğuyor.
func TestCreateManualIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	api := createMonitor(t, pool, orgID)
	web := createMonitor(t, pool, orgID)
	author := createUser(t, pool)

	detail, err := svc.CreateManual(t.Context(), orgID, manualInput(author, api.ID, web.ID))
	if err != nil {
		t.Fatalf("manuel incident açılamadı: %v", err)
	}

	if detail.Incident.IsAutoCreated {
		t.Error("is_auto_created = true, beklenen false")
	}
	if detail.Incident.AutoMonitorID != nil {
		t.Error("manuel incident'ta auto_monitor_id dolu")
	}
	if detail.Incident.Impact != store.IncidentImpactCritical {
		t.Errorf("etki = %q", detail.Incident.Impact)
	}
	if len(detail.Monitors) != 2 {
		t.Fatalf("servis sayısı = %d, beklenen 2", len(detail.Monitors))
	}
	if len(detail.Updates) != 1 || detail.Updates[0].Body != "Sorunu araştırıyoruz." {
		t.Fatalf("timeline = %+v", detail.Updates)
	}
	if detail.Updates[0].IsSystem || detail.Updates[0].AuthorID == nil {
		t.Error("ilk satır sistem gibi görünüyor")
	}
}

// Manuel incident'lar 4.1'in kısmi unique indeksinden ETKİLENMİYOR: bir org
// aynı anda istediği kadar manuel incident açabilir.
func TestCreateManualIncidentsCanCoexist(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	author := createUser(t, pool)

	for range 3 {
		if _, err := svc.CreateManual(t.Context(), orgID, manualInput(author, monitor.ID)); err != nil {
			t.Fatalf("manuel incident açılamadı: %v", err)
		}
	}

	items, err := svc.List(t.Context(), orgID, FilterOpen, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("açık incident sayısı = %d, beklenen 3", len(items))
	}
}

// Manuel incident aynı monitor'ün OTOMATİK incident'ını engellemiyor: kısıt
// yalnızca otomatik kayıtlar için (auto_monitor_id dolu olanlar).
func TestManualIncidentDoesNotBlockAutoIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	if _, err := svc.CreateManual(t.Context(), orgID,
		manualInput(createUser(t, pool), monitor.ID)); err != nil {
		t.Fatalf("manuel incident açılamadı: %v", err)
	}

	at := time.Now().UTC().Add(-time.Minute)
	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)

	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionOpened {
		t.Fatalf("action = %q, beklenen %q", action, ActionOpened)
	}
}

// YABANCI SERVİS → İŞLEM TÜMDEN GERİ ALINIYOR: sessizce eksik kapsayan bir
// incident, status page'de yanlış servisi "çalışıyor" göstermek demekti.
func TestCreateManualIncidentRollsBackOnForeignMonitor(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	otherOrg := createOrg(t, pool)

	mine := createMonitor(t, pool, orgID)
	foreign := createMonitor(t, pool, otherOrg)

	_, err := svc.CreateManual(t.Context(), orgID,
		manualInput(createUser(t, pool), mine.ID, foreign.ID))

	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("hata = %v, beklenen ValidationError", err)
	}

	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM incidents WHERE org_id = $1`, orgID).Scan(&count); err != nil {
		t.Fatalf("incident'lar sayılamadı: %v", err)
	}
	if count != 0 {
		t.Fatalf("geri alınmamış incident sayısı = %d, beklenen 0", count)
	}
}

func TestValidateManual(t *testing.T) {
	monitorID := uuid.New()
	valid := ManualInput{
		Title:      "x",
		Impact:     store.IncidentImpactMinor,
		MonitorIDs: []uuid.UUID{monitorID},
		Body:       "y",
	}

	if err := ValidateManual(valid); err != nil {
		t.Fatalf("geçerli girdi reddedildi: %v", err)
	}

	cases := []struct {
		name  string
		alter func(in *ManualInput)
		field string
	}{
		{name: "başlık yok", alter: func(in *ManualInput) { in.Title = "" }, field: "title"},
		{
			name:  "tanınmayan etki",
			alter: func(in *ManualInput) { in.Impact = store.IncidentImpact("felaket") },
			field: "impact",
		},
		{name: "servis yok", alter: func(in *ManualInput) { in.MonitorIDs = nil }, field: "monitorIds"},
		{
			name:  "aynı servis iki kez",
			alter: func(in *ManualInput) { in.MonitorIDs = []uuid.UUID{monitorID, monitorID} },
			field: "monitorIds",
		},
		{name: "ilk güncelleme yok", alter: func(in *ManualInput) { in.Body = "" }, field: "body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.alter(&in)

			var validation *ValidationError
			if err := ValidateManual(NormalizeManual(in)); !errors.As(err, &validation) {
				t.Fatalf("hata = %v, beklenen ValidationError", err)
			}
			if validation.Problems[0].Field != tc.field {
				t.Errorf("alan = %q, beklenen %q", validation.Problems[0].Field, tc.field)
			}
		})
	}
}
