-- Status page sorguları (Faz 5.1).
--
-- Dosya İKİYE AYRILIYOR ve ayrım bu adımın en önemli kuralı:
--
--   1. ORG-SCOPED (admin) — org_id ile sınırlı, 2.1'den beri geçerli kural.
--   2. PUBLIC (oturumsuz) — org_id YOK, kimliği SLUG taşıyor. Buradaki her
--      sorgu `status_page_items` ile JOIN'li: bir monitor yayımlanmamışsa
--      hiçbir public sorgudan çıkamaz. Sızıntının kapısı bir if bloğu değil,
--      sorgunun kendisi.

-- --- 1. Org-scoped (admin) --------------------------------------------------

-- name: CreateStatusPage :one
-- Organizasyonla BİRLİKTE, aynı transaction'da yazılıyor (internal/orgs).
-- Slug org slug'ından kopyalanıyor ama sütun ayrı — bkz. migration 00014.
INSERT INTO status_pages (org_id, slug)
VALUES (sqlc.arg(org_id), sqlc.arg(slug))
RETURNING *;

-- name: GetStatusPageForOrg :one
SELECT * FROM status_pages WHERE org_id = sqlc.arg(org_id);

-- name: UpdateStatusPageSettings :one
-- Sayfa ayarları (5.1'in yayım bayrağı + 5.3'ün görünüm alanları).
--
-- TEK SORGU, alan başına bir uç DEĞİL: hepsi aynı kaydın alanları ve kısmi
-- güncellemenin birleştirmesi UYGULAMADA yapılıyor (2.1'in PATCH kararı —
-- `COALESCE` ile yazılsaydı "alanı temizle" ile "alana dokunma" ayrımı
-- kaybolurdu; burada üçü de NULL olabilen alanlar olduğu için fark kritik).
UPDATE status_pages SET
    is_published = sqlc.arg(is_published),
    title        = sqlc.narg(title),
    description  = sqlc.narg(description),
    logo_url     = sqlc.narg(logo_url),
    theme_color  = sqlc.narg(theme_color),
    updated_at   = now()
WHERE org_id = sqlc.arg(org_id)
RETURNING *;

-- name: ListStatusPageGroupsForOrg :many
-- Servis grupları, kullanıcının belirlediği sırada (5.3).
SELECT g.*
FROM status_page_groups g
JOIN status_pages sp ON sp.id = g.status_page_id
WHERE sp.org_id = sqlc.arg(org_id)
ORDER BY g.position, g.name;

-- name: DeleteStatusPageGroups :exec
-- Gruplar da servis seçimiyle BİRLİKTE tümden değiştiriliyor (aşağıdaki
-- AddStatusPageItems ile aynı transaction'da). `status_page_items.group_id`
-- ON DELETE SET NULL: grubu silmek servisi yayımdan düşürmüyor.
DELETE FROM status_page_groups WHERE status_page_id = sqlc.arg(status_page_id);

-- name: CreateStatusPageGroup :one
INSERT INTO status_page_groups (status_page_id, name, position)
VALUES (sqlc.arg(status_page_id), sqlc.arg(name), sqlc.arg(position))
RETURNING *;

-- name: ListStatusPageItemsForOrg :many
-- Yayımlanan servisler, ADMİN gözüyle: monitor'ün canlı kaydından ad ve durum,
-- 5.3'ten itibaren grubuyla birlikte.
--
-- Sıra GRUP SIRASI + AD: grup içi sıralama alfabetik, çünkü 5.3 "servisleri
-- gruplama" istiyor, servis sıralama değil — sürükle-bırak bir sıra alanı
-- (`status_page_items.position`) yazma yolu olmadan eklenmiş bir sütun olurdu.
-- Gruplanmamış servisler en sonda (`position` NULL'ları sona alan sıralama).
SELECT spi.monitor_id, m.name, m.url, m.current_status, m.status_changed_at,
       g.name AS group_name
FROM status_page_items spi
JOIN status_pages sp          ON sp.id = spi.status_page_id
JOIN monitors m               ON m.id = spi.monitor_id
LEFT JOIN status_page_groups g ON g.id = spi.group_id
WHERE sp.org_id = sqlc.arg(org_id)
ORDER BY g.position NULLS LAST, g.name NULLS LAST, m.name, m.id;

-- name: DeleteStatusPageItems :exec
-- Seçimin TAMAMI değiştiriliyor (ekle/çıkar uçları yerine "listeyi bu yap").
-- Sebebi somut: iki ayrı uç, arayüzün gönderdiği seçimle sunucudaki seçimin
-- sessizce ayrışmasına açık — ve bu ayrışmanın adı "yayımlanmaması gereken bir
-- servis yayımda kaldı".
DELETE FROM status_page_items WHERE status_page_id = sqlc.arg(status_page_id);

-- name: AddStatusPageItems :execrows
-- Seçilen monitor'leri yayımlar — YALNIZCA AYNI ORGANİZASYONUNKİLERİ
-- (maintenance.sql ve incidents.sql'deki aynı desen).
--
-- Filtre sorgunun içinde: "önce doğrula, sonra yaz" deseninde araya giren bir
-- değişiklik yabancı bir monitor'ü yayımlayabilirdi. Yazılan satır sayısı
-- dönüyor; çağıran istenenden azsa işlemi TÜMDEN geri alıyor — sessizce eksik
-- yayımlanan bir liste, admin'in "yayımladım" sandığı bir servisin status
-- page'de hiç görünmemesi demekti.
-- GRUP ADLA BAĞLANIYOR, KİMLİKLE DEĞİL (5.3): gruplar her kayıtta yeniden
-- yazıldığı için kimlikleri de değişiyor; istemcinin bir önceki kayıttan
-- kalma bir grup kimliği göndermesi kaçınılmaz olurdu. Boş dize =
-- gruplanmamış (LEFT JOIN eşleşme bulamıyor → NULL). Tanınmayan bir grup adı
-- sessizce "gruplanmamış"a düşmesin diye adları SERVİS doğruluyor: grupları
-- aynı transaction'da o yazdı, yani listeyi biliyor.
--
-- İki dizi POZİSYONLA eşleştiriliyor (`WITH ORDINALITY`): i. monitor, i. grup
-- adına ait. Tek çağrıda iki argümanlı `unnest(a, b)` daha kısa olurdu ama
-- sqlc'nin ayrıştırıcısı onu tanımıyor.
INSERT INTO status_page_items (status_page_id, monitor_id, group_id)
SELECT sqlc.arg(status_page_id), m.id, g.id
FROM unnest(sqlc.arg(monitor_ids)::uuid[]) WITH ORDINALITY AS mon(monitor_id, idx)
JOIN unnest(sqlc.arg(group_names)::text[]) WITH ORDINALITY AS grp(group_name, idx)
  ON grp.idx = mon.idx
JOIN monitors m ON m.id = mon.monitor_id AND m.org_id = sqlc.arg(org_id)
LEFT JOIN status_page_groups g
       ON g.status_page_id = sqlc.arg(status_page_id) AND g.name = grp.group_name;

-- --- 2. Public (oturumsuz) --------------------------------------------------

-- name: GetPublishedStatusPage :one
-- Ziyaretçinin giriş kapısı: slug → sayfa.
--
-- `is_published` KOŞULU BURADA, uygulamada değil: yayımlanmamış bir sayfa için
-- "var ama yayımda değil" ile "hiç yok" arasında fark üretmiyoruz — ikisi de
-- satırsız dönüyor, yani ikisi de 404. Fark üretmek, denenen slug'ların hangi
-- organizasyonlara ait olduğunu dışarıya sızdırırdı (1.4'ün ErrNoAccess
-- kararıyla aynı disiplin).
--
-- Organizasyonun ADI da dönüyor: 5.1'de sayfanın başlığı o (özel başlık 5.3).
--
-- `server_now` DA BURADAN GELİYOR (7.5): cevabın "şimdi"si TEK bir kaynaktan
-- okunmalı ve o kaynak veritabanı — 3.2'nin "zamanın kaynağı veritabanı,
-- uygulama değil" kararının okuma tarafındaki karşılığı. Önceden bakım
-- pencerelerinin süzgeci (`w.ends_at > now()`) veritabanının saatini,
-- "şu an bakımda mı" ve 90 günlük pencere ise UYGULAMANIN saatini
-- kullanıyordu; aynı cevabın iki yarısı iki farklı ana bakabiliyordu.
-- Ayrı bir sorgu yerine BURAYA eklendi: fazladan gidiş-geliş yok.
SELECT sp.*, o.name AS org_name, now()::timestamptz AS server_now
FROM status_pages sp
JOIN organizations o ON o.id = sp.org_id
WHERE sp.slug = sqlc.arg(slug) AND sp.is_published;

-- name: ListPublicServices :many
-- Yayımlanan servisler ve ŞU ANKİ durumları.
--
-- Durum `monitors.current_status`tan okunuyor, ham check satırlarından YENİDEN
-- HESAPLANMIYOR (0.1 §5): doğruluk kaynağı worker'daki durum makinesi (3.3).
-- İkinci bir hesap olsaydı public sayfa ile admin panosu aynı servis için
-- farklı şey söyleyebilirdi.
--
-- URL DÖNMÜYOR: ziyaretçinin izlenen adresi bilmesi gerekmiyor ve o adres
-- çoğu zaman iç bir uç (`https://api.internal/...`). Ad ise servisin kendisi.
--
-- 5.3'ten itibaren grup adı da dönüyor ve SIRA GRUPTAN geliyor: "API" ile
-- "Pazarlama sitesi"nin hangisinin üstte olduğu kullanıcının kararı, alfabenin
-- değil. Gruplanmamış servisler en sonda.
--
-- 8.6: `last_checked_at`, `check_interval_seconds` ve `is_active` de dönüyor.
-- Bunlar BAYATLIK kararının girdisi (worker ölürse `current_status` son
-- bilinen durumu göstermeye devam eder). Yeni bir sütun EKLENMEDİ — üçü de
-- 3.2/2.1'den beri şemada duruyor.
SELECT spi.monitor_id, m.name, m.current_status, m.status_changed_at,
       m.last_checked_at, m.check_interval_seconds, m.is_active,
       g.name AS group_name
FROM status_page_items spi
JOIN monitors m ON m.id = spi.monitor_id
LEFT JOIN status_page_groups g ON g.id = spi.group_id
WHERE spi.status_page_id = sqlc.arg(status_page_id)
ORDER BY g.position NULLS LAST, g.name NULLS LAST, m.name, m.id;

-- name: ListPublicIncidents :many
-- Ziyaretçiye gösterilen kesintiler: AÇIK olanların hepsi + `since`den bu yana
-- kapanmışlar.
--
-- İKİ SÜZGEÇ VAR VE İKİSİ DE SIZINTIYA KARŞI:
--
--   (a) EXISTS(yayımlanmış bir servise bağ) — incident en az bir public
--       servisi etkilemiyorsa hiç görünmüyor. Manuel bir incident hem public
--       hem private servisi kapsıyorsa görünüyor ama etkilenen servis
--       listesinde yalnızca public olanlar dönüyor (ListPublicIncidentMonitors).
--
--   (b) auto_monitor_id IS NULL OR (o monitor yayımlanmış) — 4.1'in otomatik
--       incident BAŞLIĞI monitor ADINI içeriyor ("<ad> yanıt vermiyor"). Yani
--       başlığın kendisi bir sızıntı yüzeyi: özne yayımlanmamışsa kayıt hiç
--       çıkmamalı. (a) çoğu durumda bunu zaten kapatıyor — koşul yine de ayrıca
--       yazılı, çünkü kuralın dayanağı "bağ satırı" değil "başlıktaki ad".
--
-- YAN SONUÇ (00011'in yorumundan sapma, bilinçli): monitor'ü SİLİNMİŞ tarihsel
-- bir incident'ın bağ satırı da düşmüş olur, yani (a) elemez ve kayıt public
-- sayfada görünmez. 00011 "status page onları yine de gösterebilir" diyordu;
-- 5.1'in sızıntı kuralı bunu ters çeviriyor — public olduğu KANITLANAMAYAN bir
-- kaydı göstermek, adı başlıkta duran private bir servisi ifşa edebilir.
SELECT i.*
FROM incidents i
WHERE i.org_id = sqlc.arg(org_id)
  AND EXISTS (
      SELECT 1
      FROM incident_monitors im
      JOIN status_page_items spi
        ON spi.monitor_id = im.monitor_id
       AND spi.status_page_id = sqlc.arg(status_page_id)
      WHERE im.incident_id = i.id
  )
  AND (
      i.auto_monitor_id IS NULL
      OR EXISTS (
          SELECT 1 FROM status_page_items spi
          WHERE spi.status_page_id = sqlc.arg(status_page_id)
            AND spi.monitor_id = i.auto_monitor_id
      )
  )
  AND (i.resolved_at IS NULL OR i.resolved_at >= sqlc.arg(since)::timestamptz)
ORDER BY i.started_at DESC, i.id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListPublicIncidentMonitors :many
-- Etkilenen servisler — YALNIZCA YAYIMLANANLAR.
--
-- `ListIncidentMonitors`ın (4.2) public ikizi ve tek farkı bu JOIN: private bir
-- servisin adı, kapsayan incident public olsa bile listeye giremez.
SELECT im.incident_id, m.id AS monitor_id, m.name, m.current_status
FROM incident_monitors im
JOIN status_page_items spi
  ON spi.monitor_id = im.monitor_id
 AND spi.status_page_id = sqlc.arg(status_page_id)
JOIN monitors m ON m.id = im.monitor_id
WHERE im.incident_id = ANY(sqlc.arg(incident_ids)::uuid[])
ORDER BY m.name, m.id;

-- name: ListPublicIncidentUpdates :many
-- Gösterilen incident'ların timeline'ı (4.2'nin anlatısı).
--
-- Kimlik alanı YOK: `author_id` ziyaretçiye dönmüyor (org üyelerinin kimlikleri
-- public bir yüzeyde işi olmayan bir veri), `is_system` duruyor — "bunu sistem
-- yazdı" ile "bir mühendis yazdı" ayrımı okuyucu için anlamlı.
--
-- İncident süzgeci ÇAĞIRANDAN geliyor: kimlikler ListPublicIncidents'ın
-- döndürdüğü küme, yani yukarıdaki iki süzgeçten geçmiş olanlar.
SELECT u.incident_id, u.id, u.status, u.body, u.is_system, u.created_at
FROM incident_updates u
WHERE u.incident_id = ANY(sqlc.arg(incident_ids)::uuid[])
ORDER BY u.incident_id, u.created_at, u.id;

-- name: ListPublicMaintenanceWindows :many
-- Ziyaretçiye gösterilen bakım pencereleri: AKTİF ve YAKLAŞAN olanlar.
--
-- Geçmiş pencereler dışarıda: status page'in sorusu "şu an ve yakında ne
-- olacak". Geçmiş bakımlar 5.2'nin 90 günlük barlarında desenle görünecek.
--
-- Süzgeç yine yayımlanan servisler üzerinden; kapsadığı servislerin hiçbiri
-- public değilse pencere de görünmüyor (başlığı "prod-db bakımı" olabilir).
SELECT w.*
FROM maintenance_windows w
WHERE w.org_id = sqlc.arg(org_id)
  AND w.ends_at > now()
  AND EXISTS (
      SELECT 1
      FROM maintenance_window_monitors mwm
      JOIN status_page_items spi
        ON spi.monitor_id = mwm.monitor_id
       AND spi.status_page_id = sqlc.arg(status_page_id)
      WHERE mwm.window_id = w.id
  )
ORDER BY w.starts_at, w.id
LIMIT sqlc.arg(row_limit);

-- name: ListPublicStatusPageGroups :many
-- Grupların GÖSTERİM SIRASI (5.3).
--
-- Boş gruplar da dönüyor; hangisinin çizileceğine arayüz karar veriyor
-- (servisi olmayan bir grup başlığı gösterilmiyor). Sorgunun burada eleme
-- yapması, "grup var ama servisi yok" durumunu iki farklı yerde iki farklı
-- kuralla çözmek olurdu.
SELECT g.*
FROM status_page_groups g
WHERE g.status_page_id = sqlc.arg(status_page_id)
ORDER BY g.position, g.name;

-- name: ListPublicDailyUptime :many
-- 90 GÜNLÜK GRAFİĞİN VERİSİ (5.2) — GÜNLÜK ÖZETLERDEN, ham satırlardan DEĞİL.
--
-- Bu bir optimizasyon değil tasarımın kendisi (0.1 §6): ham `check_results`
-- 30 gün sonra BUDANIYOR (3.4), yani 90 günün ilk 60'ı yalnızca burada yaşıyor.
-- Hacim farkı da bu: monitor başına ~130 bin ham satır ↔ 90 özet satırı.
--
-- Uptime BURADA HESAPLANMIYOR, sayılar dönüyor: formül 0.1'de bağlayıcı ve
-- 4.3'te dört parçaya çıktı ((success + degraded) / (total - maintenance)).
-- `total = 0` olan gün "veri yok" (null) — %100 DEĞİL, ve o ayrımı yapabilmek
-- için uygulamanın ham sayıları görmesi gerekiyor.
--
-- Süzgeç yine `status_page_items`: yayımlanmamış bir servisin geçmişi de
-- yayımlanmamıştır.
SELECT r.monitor_id, r.bucket_date, r.total_checks, r.success_checks,
       r.degraded_checks, r.failure_checks, r.maintenance_checks
FROM uptime_rollup_daily r
JOIN status_page_items spi
  ON spi.monitor_id = r.monitor_id
 AND spi.status_page_id = sqlc.arg(status_page_id)
WHERE r.bucket_date >= sqlc.arg(since_date)::date
ORDER BY r.monitor_id, r.bucket_date;

-- name: ListPublicIncidentDays :many
-- Hangi kesinti hangi GÜNE düşüyor (5.2'nin bar üstü bilgisi).
--
-- Ayrı bir sorgu, `ListPublicIncidents`in cevabından türetilmiyor: o liste son
-- 30 günü kapsıyor, grafik ise 90 günü. Türetilseydi 31-90. günlerin barları
-- "o gün kesinti yoktu" diye görünürdü — grafiğin tam olarak anlatmaması
-- gereken şey.
--
-- Bir kesinti birden fazla güne yayılabilir, o yüzden gün dizisi üretiliyor.
-- Tarihler UTC'de: `uptime_rollup_daily.bucket_date` de öyle (3.4) ve iki
-- taraf aynı takvim gününden bahsetmeli.
--
-- Sızıntı süzgeçleri ListPublicIncidents ile AYNI ve aynı gerekçeyle: (a) bağ
-- yayımlanmış bir servise, (b) otomatik incident'ın öznesi yayımlanmış olmalı —
-- başlık monitor adını içeriyor (4.1).
SELECT im.monitor_id,
       day::date AS day,
       i.id AS incident_id,
       i.title,
       i.impact,
       -- Açık cast: cast'siz hâlde sqlc kolonun tipini çıkaramayıp `interface{}`
       -- üretiyor (4.2'nin `::uuid`/`::timestamptz` cast'leriyle aynı sebep).
       (i.resolved_at IS NULL)::boolean AS is_open
FROM incidents i
JOIN incident_monitors im ON im.incident_id = i.id
JOIN status_page_items spi
  ON spi.monitor_id = im.monitor_id
 AND spi.status_page_id = sqlc.arg(status_page_id)
CROSS JOIN LATERAL generate_series(
    greatest((i.started_at AT TIME ZONE 'UTC')::date, sqlc.arg(since_date)::date),
    least(
        (coalesce(i.resolved_at, now()) AT TIME ZONE 'UTC')::date,
        (now() AT TIME ZONE 'UTC')::date
    ),
    interval '1 day'
) AS day
WHERE i.org_id = sqlc.arg(org_id)
  AND (i.resolved_at IS NULL OR (i.resolved_at AT TIME ZONE 'UTC')::date >= sqlc.arg(since_date)::date)
  AND (
      i.auto_monitor_id IS NULL
      OR EXISTS (
          SELECT 1 FROM status_page_items spi2
          WHERE spi2.status_page_id = sqlc.arg(status_page_id)
            AND spi2.monitor_id = i.auto_monitor_id
      )
  )
ORDER BY im.monitor_id, day, i.started_at DESC, i.id;

-- name: ListPublicMaintenanceMonitors :many
-- Bakım pencerelerinin kapsadığı servisler — yalnızca yayımlananlar
-- (ListPublicIncidentMonitors ile aynı gerekçe).
SELECT mwm.window_id, m.id AS monitor_id, m.name, m.current_status
FROM maintenance_window_monitors mwm
JOIN status_page_items spi
  ON spi.monitor_id = mwm.monitor_id
 AND spi.status_page_id = sqlc.arg(status_page_id)
JOIN monitors m ON m.id = mwm.monitor_id
WHERE mwm.window_id = ANY(sqlc.arg(window_ids)::uuid[])
ORDER BY m.name, m.id;
