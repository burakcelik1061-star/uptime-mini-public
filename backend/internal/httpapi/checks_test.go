package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeCheckRunner, kontrol çalıştırıcısının HTTP katmanı için sahtesi.
//
// Gerçek ağ davranışı burada DEĞİL, internal/checker'ın httptest'li
// testlerinde ölçülüyor. Buradaki konu sözleşme: yetki, hedefin doğru
// kurulması, sonucun gövdeye çevrilmesi ve "kontrol çalışmadı" hâli.
type fakeCheckRunner struct {
	result checker.Result
	err    error
	// lastTarget, handler'ın monitor kaydından ürettiği hedef.
	lastTarget checker.Target
	calls      int
}

func (f *fakeCheckRunner) Run(_ context.Context, target checker.Target) (checker.Result, error) {
	f.calls++
	f.lastTarget = target

	if f.err != nil {
		return checker.Result{}, f.err
	}

	return f.result, nil
}

// fakeResultStore, results.Store'un bellek içi karşılığı.
//
// SAHTE OLAN YALNIZCA SAKLAMA: üzerine GERÇEK results.Service kuruluyor, yani
// checker.Result → satır çevrimi (null'lar, eksik hata tipinin doldurulması)
// testte de üretimdeki kodla yapılıyor. SQL'in org izolasyonu ve şema
// kısıtları burada değil, internal/results'un gerçek veritabanı testlerinde.
type fakeResultStore struct {
	rows     []store.CheckResult
	nextID   int64
	failWith error
}

func newFakeResultStore() *fakeResultStore {
	return &fakeResultStore{nextID: 1}
}

func (f *fakeResultStore) InsertCheckResult(
	_ context.Context, arg store.InsertCheckResultParams,
) (store.CheckResult, error) {
	if f.failWith != nil {
		return store.CheckResult{}, f.failWith
	}

	row := store.CheckResult{
		ID:             f.nextID,
		MonitorID:      arg.MonitorID,
		CheckedAt:      arg.CheckedAt,
		Outcome:        arg.Outcome,
		TriggeredBy:    arg.TriggeredBy,
		HttpStatus:     arg.HttpStatus,
		ResponseTimeMs: arg.ResponseTimeMs,
		ErrorType:      arg.ErrorType,
		ErrorMessage:   arg.ErrorMessage,
	}
	f.nextID++
	f.rows = append(f.rows, row)

	return row, nil
}

// forMonitor, en yeniden eskiye sıralı satırları döner (SQL'in ORDER BY'ı).
func (f *fakeResultStore) forMonitor(monitorID uuid.UUID) []store.CheckResult {
	rows := make([]store.CheckResult, 0, len(f.rows))
	for _, row := range f.rows {
		if row.MonitorID == monitorID {
			rows = append(rows, row)
		}
	}

	slices.SortFunc(rows, func(a, b store.CheckResult) int {
		return b.CheckedAt.Compare(a.CheckedAt)
	})

	return rows
}

func (f *fakeResultStore) ListRecentChecks(
	_ context.Context, arg store.ListRecentChecksParams,
) ([]store.CheckResult, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	rows := f.forMonitor(arg.MonitorID)
	if int32(len(rows)) > arg.RowLimit {
		rows = rows[:arg.RowLimit]
	}

	return rows, nil
}

func (f *fakeResultStore) ListChecksInRange(
	_ context.Context, arg store.ListChecksInRangeParams,
) ([]store.CheckResult, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	filtered := make([]store.CheckResult, 0, len(f.rows))
	for _, row := range f.forMonitor(arg.MonitorID) {
		// [from, to) — yarı açık.
		if !row.CheckedAt.Before(arg.FromTime) && row.CheckedAt.Before(arg.ToTime) {
			filtered = append(filtered, row)
		}
	}

	if int32(len(filtered)) > arg.RowLimit {
		filtered = filtered[:arg.RowLimit]
	}

	return filtered, nil
}

func (f *fakeResultStore) SummarizeChecksInRange(
	_ context.Context, arg store.SummarizeChecksInRangeParams,
) (store.SummarizeChecksInRangeRow, error) {
	if f.failWith != nil {
		return store.SummarizeChecksInRangeRow{}, f.failWith
	}

	var row store.SummarizeChecksInRangeRow

	for _, check := range f.forMonitor(arg.MonitorID) {
		if check.CheckedAt.Before(arg.FromTime) || !check.CheckedAt.Before(arg.ToTime) {
			continue
		}

		// Manuel kontroller uptime sayımlarına GİRMİYOR (bkz. sorgu).
		if check.TriggeredBy == store.CheckTriggerManual {
			row.ManualChecks++
			continue
		}

		row.ScheduledChecks++

		switch check.Outcome {
		case store.CheckOutcomeSuccess:
			row.SuccessChecks++
		case store.CheckOutcomeDegraded:
			row.DegradedChecks++
		case store.CheckOutcomeFailure:
			row.FailureChecks++
		}

		if check.ResponseTimeMs != nil {
			row.ResponseTimeTotalMs += int64(*check.ResponseTimeMs)
			row.ResponseTimeSamples++
		}
	}

	return row, nil
}

func newFakeResultService(t *testing.T, storage *fakeResultStore) *results.Service {
	t.Helper()

	return results.NewService(storage, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func successResult() checker.Result {
	return checker.Result{
		Outcome:      store.CheckOutcomeSuccess,
		HTTPStatus:   200,
		ResponseTime: 123 * time.Millisecond,
		CheckedAt:    time.Now().UTC(),
	}
}

// checkFixture, monitor fixture'ına sahte çalıştırıcıyı ve sonuç deposunu
// ekler.
type checkFixture struct {
	monitorFixture
	runner  *fakeCheckRunner
	storage *fakeResultStore
	monitor monitorResponse
}

func newCheckFixture(t *testing.T) checkFixture {
	t.Helper()

	deps := newTestDeps(t)

	runner := &fakeCheckRunner{result: successResult()}
	deps.Checks = runner

	storage := newFakeResultStore()
	deps.Results = newFakeResultService(t, storage)

	fixture := newMonitorFixtureWithDeps(t, deps)

	return checkFixture{
		monitorFixture: fixture,
		runner:         runner,
		storage:        storage,
		monitor:        fixture.createMonitor(t, validMonitorBody),
	}
}

func (f checkFixture) checkPath() string {
	return f.path("/" + f.monitor.ID + "/check")
}

func (f checkFixture) historyPath(query string) string {
	path := f.path("/" + f.monitor.ID + "/checks")
	if query != "" {
		path += "?" + query
	}

	return path
}

func decodeCheck(t *testing.T, body []byte) checkResponse {
	t.Helper()

	var wrapper struct {
		Check checkResponse `json:"check"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Check
}

func decodeChecks(t *testing.T, body []byte) []checkResponse {
	t.Helper()

	var wrapper struct {
		Checks []checkResponse `json:"checks"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Checks
}

// --- Manuel kontrol ----------------------------------------------------------

func TestRunCheckReturnsResult(t *testing.T) {
	fixture := newCheckFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	check := decodeCheck(t, rec.Body.Bytes())
	if check.Outcome != string(store.CheckOutcomeSuccess) {
		t.Errorf("outcome = %q", check.Outcome)
	}
	if check.HTTPStatus == nil || *check.HTTPStatus != 200 {
		t.Errorf("httpStatus = %v", check.HTTPStatus)
	}
	if check.ResponseTimeMs == nil || *check.ResponseTimeMs != 123 {
		t.Errorf("responseTimeMs = %v", check.ResponseTimeMs)
	}
	if check.ErrorType != nil || check.ErrorMessage != nil {
		t.Errorf("başarılı kontrolde hata alanları dolu: %+v", check)
	}
	if check.ID == "" {
		t.Error("kaydedilen satırın kimliği dönmemiş")
	}
}

// 2.3'ün kararı: manuel kontrol de zaman serisine yazılıyor, ama KAYNAĞI
// işaretli (`manual`) — 3.3'ün durum makinesi ve 3.4'ün toplulaştırması
// ayrımı yapabilsin.
func TestRunCheckIsStoredAsManual(t *testing.T) {
	fixture := newCheckFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	if len(fixture.storage.rows) != 1 {
		t.Fatalf("kayıt sayısı = %d, beklenen 1", len(fixture.storage.rows))
	}

	row := fixture.storage.rows[0]
	if row.TriggeredBy != store.CheckTriggerManual {
		t.Errorf("kaynak = %q, beklenen manual", row.TriggeredBy)
	}
	if row.MonitorID.String() != fixture.monitor.ID {
		t.Errorf("monitor = %s", row.MonitorID)
	}

	if got := decodeCheck(t, rec.Body.Bytes()).TriggeredBy; got != string(store.CheckTriggerManual) {
		t.Errorf("cevapta kaynak = %q", got)
	}
}

// Hedef, MONİTOR KAYDINDAN kuruluyor: istemci kontrol parametrelerini
// isteğinde geçemiyor (geçebilseydi kayıtlı yapılandırmayı atlayan bir
// "istediğim adrese istek attır" ucu doğardı).
func TestRunCheckUsesStoredMonitorSettings(t *testing.T) {
	fixture := newCheckFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	target := fixture.runner.lastTarget
	if target.URL != "https://example.com/health" {
		t.Errorf("url = %q", target.URL)
	}
	if target.Method != store.MonitorMethodGET {
		t.Errorf("metot = %q", target.Method)
	}
	if target.Timeout != 10*time.Second {
		t.Errorf("timeout = %v", target.Timeout)
	}
	if target.ExpectedStatusMin != 200 || target.ExpectedStatusMax != 299 {
		t.Errorf("status aralığı = %d-%d", target.ExpectedStatusMin, target.ExpectedStatusMax)
	}
	if target.DegradedAfter != 0 {
		t.Errorf("degradedAfter = %v, eşik verilmemişti", target.DegradedAfter)
	}
}

// Kontrolün BAŞARISIZ olması isteğin başarısız olması demek değil: 200 +
// sonuç. Hata alanları dolu geliyor, status null.
func TestFailedCheckIsStillHTTP200(t *testing.T) {
	fixture := newCheckFixture(t)
	fixture.runner.result = checker.Result{
		Outcome:      store.CheckOutcomeFailure,
		ResponseTime: 5 * time.Second,
		ErrorType:    store.CheckErrorTypeTimeout,
		ErrorMessage: "hedef zamanında cevap vermedi",
		CheckedAt:    time.Now().UTC(),
	}

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	check := decodeCheck(t, rec.Body.Bytes())
	if check.Outcome != string(store.CheckOutcomeFailure) {
		t.Errorf("outcome = %q", check.Outcome)
	}
	if check.ErrorType == nil || *check.ErrorType != string(store.CheckErrorTypeTimeout) {
		t.Errorf("errorType = %v", check.ErrorType)
	}
	// Cevap alınamadı: status UYDURULMUYOR.
	if check.HTTPStatus != nil {
		t.Errorf("httpStatus = %v, null olmalıydı", check.HTTPStatus)
	}
	// Başarısız kontrol de saklanıyor.
	if len(fixture.storage.rows) != 1 {
		t.Errorf("kayıt sayısı = %d", len(fixture.storage.rows))
	}
}

// Kontrol hiç çalışmadıysa (istemci bağlantıyı kesti, süreç kapanıyor)
// SONUÇ ÜRETİLMİYOR ve zaman serisine de bir şey YAZILMIYOR.
func TestCheckNotRunIsServiceUnavailable(t *testing.T) {
	fixture := newCheckFixture(t)
	fixture.runner.err = fmt.Errorf("%w: context canceled", checker.ErrCheckNotRun)

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, beklenen 503 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeCheckNotRun {
		t.Errorf("code = %q", code)
	}
	if len(fixture.storage.rows) != 0 {
		t.Errorf("çalışmayan kontrol kaydedilmiş: %+v", fixture.storage.rows)
	}
}

func TestCheckRunnerErrorBecomesInternalError(t *testing.T) {
	fixture := newCheckFixture(t)
	fixture.runner.err = errors.New("beklenmeyen")

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
}

// Ölçüm alınmış ama YAZILAMAMIŞSA istek de başarısız: istemciye
// "kaydedildi" izlenimi verilmemeli.
func TestStorageFailureFailsTheRequest(t *testing.T) {
	fixture := newCheckFixture(t)
	fixture.storage.failWith = errors.New("veritabanı yok")

	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
}

// YETKİ: manuel kontrol yazma tarafında — sunucuyu dışarıya istek attırıyor.
func TestRunCheckRequiresAdminRole(t *testing.T) {
	fixture := newCheckFixture(t)

	// Salt-okunur üye tetikleyemez.
	rec := do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.memberTok))
	if rec.Code != http.StatusForbidden {
		t.Errorf("üye için status = %d, beklenen 403 (%s)", rec.Code, rec.Body.String())
	}

	// Başka organizasyonun kullanıcısı için monitor "yok".
	rec = do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", bearer(fixture.outsider))
	if rec.Code != http.StatusNotFound {
		t.Errorf("yabancı için status = %d, beklenen 404", rec.Code)
	}

	// Tokensiz.
	rec = do(t, fixture.router, http.MethodPost, fixture.checkPath(), "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tokensiz status = %d, beklenen 401", rec.Code)
	}

	// Hiçbiri kontrolü ÇALIŞTIRMAMIŞ olmalı: yetkisiz bir istek sunucuya
	// dışarıya istek attıramaz.
	if fixture.runner.calls != 0 {
		t.Errorf("yetkisiz isteklerde kontrol çalıştı: %d kez", fixture.runner.calls)
	}
}

func TestRunCheckUnknownMonitorIsNotFound(t *testing.T) {
	fixture := newCheckFixture(t)

	unknown := do(t, fixture.router, http.MethodPost,
		fixture.path("/"+uuid.NewString()+"/check"), "", bearer(fixture.ownerTok))
	malformed := do(t, fixture.router, http.MethodPost,
		fixture.path("/bu-uuid-degil/check"), "", bearer(fixture.ownerTok))

	if unknown.Code != http.StatusNotFound || malformed.Code != http.StatusNotFound {
		t.Fatalf("status = %d / %d, ikisi de 404 olmalı", unknown.Code, malformed.Code)
	}
	if fixture.runner.calls != 0 {
		t.Errorf("olmayan monitor için kontrol çalıştı")
	}
}

// Yavaş eşiği olan bir monitor'de hedefin DegradedAfter'ı doluyor.
func TestRunCheckPassesDegradedThreshold(t *testing.T) {
	fixture := newCheckFixture(t)

	monitor := fixture.createMonitor(t,
		`{"name":"Yavas","url":"https://example.com","degradedResponseMs":250}`)

	rec := do(t, fixture.router, http.MethodPost,
		fixture.path("/"+monitor.ID+"/check"), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	if got := fixture.runner.lastTarget.DegradedAfter; got != 250*time.Millisecond {
		t.Errorf("degradedAfter = %v", got)
	}
}

// Uçtan uca: gerçek Checker + gerçek bir httptest hedefi. Sahte
// çalıştırıcının atladığı zinciri (monitor kaydı → hedef → HTTP isteği →
// gövde → saklama) bir kez de bütün hâlinde ölçüyor.
func TestRunCheckAgainstRealTarget(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	deps := newTestDeps(t)
	deps.Checks = checker.New(safedial.Policy{AllowPrivate: true})
	storage := newFakeResultStore()
	deps.Results = newFakeResultService(t, storage)

	fixture := newMonitorFixtureWithDeps(t, deps)

	monitor := fixture.createMonitor(t,
		`{"name":"Yerel","url":"`+target.URL+`","expectedStatusMin":200,"expectedStatusMax":299}`)

	rec := do(t, fixture.router, http.MethodPost,
		fixture.path("/"+monitor.ID+"/check"), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	check := decodeCheck(t, rec.Body.Bytes())
	if check.Outcome != string(store.CheckOutcomeSuccess) {
		t.Errorf("outcome = %q (%v)", check.Outcome, check.ErrorMessage)
	}
	if check.HTTPStatus == nil || *check.HTTPStatus != 200 {
		t.Errorf("httpStatus = %v", check.HTTPStatus)
	}
	if len(storage.rows) != 1 {
		t.Errorf("kayıt sayısı = %d", len(storage.rows))
	}
}

// --- Kontrol geçmişi ----------------------------------------------------------

// record, geçmiş testleri için doğrudan depoya sonuç yazar.
func (f checkFixture) record(t *testing.T, at time.Time) {
	t.Helper()

	monitorID, err := uuid.Parse(f.monitor.ID)
	if err != nil {
		t.Fatalf("monitor id çözümlenemedi: %v", err)
	}

	if _, err := f.storage.InsertCheckResult(t.Context(), store.InsertCheckResultParams{
		MonitorID:      monitorID,
		CheckedAt:      at,
		Outcome:        store.CheckOutcomeSuccess,
		TriggeredBy:    store.CheckTriggerScheduled,
		HttpStatus:     ptr(int32(200)),
		ResponseTimeMs: ptr(int32(50)),
	}); err != nil {
		t.Fatalf("kayıt yazılamadı: %v", err)
	}
}

func ptr[T any](value T) *T { return &value }

func TestListChecksReturnsNewestFirst(t *testing.T) {
	fixture := newCheckFixture(t)

	base := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		fixture.record(t, base.Add(time.Duration(i)*time.Minute))
	}

	rec := do(t, fixture.router, http.MethodGet, fixture.historyPath(""), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	checks := decodeChecks(t, rec.Body.Bytes())
	if len(checks) != 3 {
		t.Fatalf("kayıt sayısı = %d", len(checks))
	}
	if !checks[0].CheckedAt.After(checks[2].CheckedAt) {
		t.Errorf("sıralama yeniden eskiye değil: %+v", checks)
	}
}

// Geçmiş OKUMA tarafında: salt-okunur üye de görebiliyor.
func TestListChecksIsReadableByMember(t *testing.T) {
	fixture := newCheckFixture(t)
	fixture.record(t, time.Now().UTC())

	rec := do(t, fixture.router, http.MethodGet, fixture.historyPath(""), "", bearer(fixture.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("üye için status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	// Yabancı organizasyon yine 404.
	rec = do(t, fixture.router, http.MethodGet, fixture.historyPath(""), "", bearer(fixture.outsider))
	if rec.Code != http.StatusNotFound {
		t.Errorf("yabancı için status = %d, beklenen 404", rec.Code)
	}
}

func TestListChecksEmptyIsEmptyArray(t *testing.T) {
	fixture := newCheckFixture(t)

	rec := do(t, fixture.router, http.MethodGet, fixture.historyPath(""), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Boş liste `null` değil `[]` (bkz. monitor listesi).
	if body := rec.Body.String(); !strings.Contains(body, `"checks":[]`) {
		t.Errorf("gövde = %s", body)
	}
}

func TestListChecksHonoursLimit(t *testing.T) {
	fixture := newCheckFixture(t)

	base := time.Now().UTC().Add(-time.Hour)
	for i := range 5 {
		fixture.record(t, base.Add(time.Duration(i)*time.Minute))
	}

	rec := do(t, fixture.router, http.MethodGet, fixture.historyPath("limit=2"), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if checks := decodeChecks(t, rec.Body.Bytes()); len(checks) != 2 {
		t.Errorf("kayıt sayısı = %d, beklenen 2", len(checks))
	}
}

// Aralık YARI AÇIK: [from, to). Üst sınıra düşen kayıt dışarıda kalıyor.
func TestListChecksRangeIsHalfOpen(t *testing.T) {
	fixture := newCheckFixture(t)

	start := time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
	for _, at := range []time.Time{start, start.Add(30 * time.Second), start.Add(time.Minute)} {
		fixture.record(t, at)
	}

	query := url.Values{
		"from": {start.Format(time.RFC3339)},
		"to":   {start.Add(time.Minute).Format(time.RFC3339)},
	}

	rec := do(t, fixture.router, http.MethodGet, fixture.historyPath(query.Encode()), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	if checks := decodeChecks(t, rec.Body.Bytes()); len(checks) != 2 {
		t.Errorf("kayıt sayısı = %d, beklenen 2 (üst sınır hariç)", len(checks))
	}
}

// Sorgu parametreleri gövde değil: hatalı olduklarında 400 (422 değil).
func TestListChecksRejectsBadQuery(t *testing.T) {
	fixture := newCheckFixture(t)

	now := time.Now().UTC()
	tests := []struct {
		name  string
		query string
		field string
	}{
		{"sayı olmayan limit", "limit=abc", "limit"},
		{"sıfır limit", "limit=0", "limit"},
		{"bozuk from", "from=dun&to=" + url.QueryEscape(now.Format(time.RFC3339)), "from"},
		{"yalnız from", "from=" + url.QueryEscape(now.Format(time.RFC3339)), "from"},
		{
			"ters aralık",
			"from=" + url.QueryEscape(now.Format(time.RFC3339)) +
				"&to=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)),
			"to",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, fixture.router, http.MethodGet,
				fixture.historyPath(tt.query), "", bearer(fixture.ownerTok))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, beklenen 400 (%s)", rec.Code, rec.Body.String())
			}

			body := decodeErrorResponse(t, rec)
			if body.Error.Code != codeInvalidQuery {
				t.Errorf("code = %q", body.Error.Code)
			}
			if len(body.Error.Fields) == 0 || body.Error.Fields[0].Field != tt.field {
				t.Errorf("alan hataları = %+v", body.Error.Fields)
			}
		})
	}
}

// Üst sınırı aşan limit HATA DEĞİL: servis kırpıyor.
func TestListChecksClampsHugeLimit(t *testing.T) {
	fixture := newCheckFixture(t)
	fixture.record(t, time.Now().UTC())

	rec := do(t, fixture.router, http.MethodGet,
		fixture.historyPath("limit=100000"), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}
}

// --- Özet (2.4) ---------------------------------------------------------------

func decodeSummary(t *testing.T, body []byte) summaryResponse {
	t.Helper()

	var wrapper struct {
		Summary summaryResponse `json:"summary"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Summary
}

// recordOutcome, özet testleri için belirli sonuçlu bir satır yazar.
func (f checkFixture) recordOutcome(
	t *testing.T, at time.Time, trigger store.CheckTrigger, outcome store.CheckOutcome,
) {
	t.Helper()

	monitorID, err := uuid.Parse(f.monitor.ID)
	if err != nil {
		t.Fatalf("monitor id çözümlenemedi: %v", err)
	}

	params := store.InsertCheckResultParams{
		MonitorID:      monitorID,
		CheckedAt:      at,
		Outcome:        outcome,
		TriggeredBy:    trigger,
		HttpStatus:     ptr(int32(200)),
		ResponseTimeMs: ptr(int32(100)),
	}

	if outcome == store.CheckOutcomeFailure {
		params.HttpStatus = nil
		params.ResponseTimeMs = nil
		params.ErrorType = store.NullCheckErrorType{
			CheckErrorType: store.CheckErrorTypeTimeout,
			Valid:          true,
		}
	}

	if _, err := f.storage.InsertCheckResult(t.Context(), params); err != nil {
		t.Fatalf("kayıt yazılamadı: %v", err)
	}
}

func (f checkFixture) summaryPath(from, to time.Time) string {
	query := url.Values{
		"from": {from.Format(time.RFC3339)},
		"to":   {to.Format(time.RFC3339)},
	}

	return f.path("/" + f.monitor.ID + "/summary?" + query.Encode())
}

func TestSummaryReturnsUptime(t *testing.T) {
	fixture := newCheckFixture(t)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	for i := range 3 {
		fixture.recordOutcome(t, from.Add(time.Duration(i)*time.Minute),
			store.CheckTriggerScheduled, store.CheckOutcomeSuccess)
	}
	fixture.recordOutcome(t, from.Add(4*time.Minute),
		store.CheckTriggerScheduled, store.CheckOutcomeFailure)

	rec := do(t, fixture.router, http.MethodGet, fixture.summaryPath(from, to), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	summary := decodeSummary(t, rec.Body.Bytes())
	if summary.ScheduledChecks != 4 || summary.SuccessChecks != 3 || summary.FailureChecks != 1 {
		t.Errorf("sayımlar = %+v", summary)
	}
	if summary.UptimePercent == nil || *summary.UptimePercent != 75 {
		t.Errorf("uptime = %v, beklenen 75", summary.UptimePercent)
	}
	// Ortalamaya yalnızca ölçümü olan (başarılı) kontroller giriyor.
	if summary.AvgResponseTimeMs == nil || *summary.AvgResponseTimeMs != 100 {
		t.Errorf("ortalama = %v", summary.AvgResponseTimeMs)
	}
}

// HİÇ zamanlanmış kontrol yoksa uptime null — %100 DEĞİL. Sözleşmede de
// böyle görünmeli (arayüz "veri yok" diyebilsin).
func TestSummaryWithoutChecksIsNullUptime(t *testing.T) {
	fixture := newCheckFixture(t)

	from := time.Now().UTC().Add(-time.Hour)
	// Aralıkta yalnızca MANUEL kontrol var: uptime yine bilinmiyor.
	fixture.recordOutcome(t, from.Add(time.Minute), store.CheckTriggerManual, store.CheckOutcomeSuccess)

	rec := do(t, fixture.router, http.MethodGet,
		fixture.summaryPath(from, from.Add(time.Hour)), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	summary := decodeSummary(t, rec.Body.Bytes())
	if summary.UptimePercent != nil {
		t.Errorf("uptime = %v, null olmalıydı", *summary.UptimePercent)
	}
	if summary.ManualChecks != 1 {
		t.Errorf("manuel sayısı = %d", summary.ManualChecks)
	}
	if summary.ScheduledChecks != 0 {
		t.Errorf("zamanlanmış sayısı = %d", summary.ScheduledChecks)
	}
}

// Aralık ZORUNLU: sessiz bir varsayılan, arayüzün seçtiği pencereyle
// sunucunun varsayımının ayrışmasına kapı açardı.
func TestSummaryRequiresRange(t *testing.T) {
	fixture := newCheckFixture(t)

	rec := do(t, fixture.router, http.MethodGet,
		fixture.path("/"+fixture.monitor.ID+"/summary"), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, beklenen 400 (%s)", rec.Code, rec.Body.String())
	}

	body := decodeErrorResponse(t, rec)
	if body.Error.Code != codeInvalidQuery {
		t.Errorf("code = %q", body.Error.Code)
	}
	if len(body.Error.Fields) != 2 {
		t.Errorf("iki alan hatası bekleniyordu: %+v", body.Error.Fields)
	}
}

func TestSummaryIsOrgScoped(t *testing.T) {
	fixture := newCheckFixture(t)

	from := time.Now().UTC().Add(-time.Hour)

	// Salt-okunur üye görebilmeli.
	rec := do(t, fixture.router, http.MethodGet,
		fixture.summaryPath(from, from.Add(time.Hour)), "", bearer(fixture.memberTok))
	if rec.Code != http.StatusOK {
		t.Errorf("üye için status = %d, beklenen 200", rec.Code)
	}

	// Yabancı organizasyon görmemeli.
	rec = do(t, fixture.router, http.MethodGet,
		fixture.summaryPath(from, from.Add(time.Hour)), "", bearer(fixture.outsider))
	if rec.Code != http.StatusNotFound {
		t.Errorf("yabancı için status = %d, beklenen 404", rec.Code)
	}
}

func TestListChecksUnknownMonitorIsNotFound(t *testing.T) {
	fixture := newCheckFixture(t)

	rec := do(t, fixture.router, http.MethodGet,
		fixture.path("/"+uuid.NewString()+"/checks"), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, beklenen 404 (%s)", rec.Code, rec.Body.String())
	}
}
