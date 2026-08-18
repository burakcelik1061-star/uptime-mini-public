-- Incident sorguları (Faz 4.1).
--
-- Bu iki sorgu WORKER tarafından çağrılıyor ve org_id İSTEMİYOR: organizasyon
-- monitor kaydından türetiliyor (rollups.sql ile aynı desen). Org-scoped okuma
-- uçları 4.2'de gelecek ve orada koşul yine org_id ile yazılacak.
--
-- İkisinin de ortak bir numarası var: kesintinin GERÇEK zamanı, eşiğin dolduğu
-- an değil. Durum makinesi (3.3) bir monitor'ü ancak `failure_threshold`
-- ardışık hatadan SONRA `down` ilan ediyor — yani 3 eşikli, 60 saniye aralıklı
-- bir monitor'de kesinti ilan edildiğinde servis zaten 2 dakikadır cevap
-- vermiyor. `started_at`i o ana yazmak, her kesintiyi sistematik olarak
-- (eşik-1) × aralık kadar KISA gösterirdi. Serinin ilk başarısız kontrolü ise
-- uydurulmuş bir tahmin değil, elimizdeki gerçek bir ölçüm.
--
-- Seri şöyle bulunuyor: son `streak_length` ZAMANLANMIŞ kontrol (sayaç
-- değerinin kendisi) — bunların hepsi tanım gereği aynı türden, çünkü sayacı
-- araya giren tek bir farklı sonuç sıfırlıyor. Manuel kontroller dışarıda:
-- durum makinesini de zaten onlar tetiklemiyor (2.3).

-- name: OpenAutoIncident :one
-- Bir monitor DOWN'a geçtiğinde otomatik incident açar.
--
-- "AYNI KESİNTİ İÇİN İKİNCİ INCIDENT AÇILMASIN" GARANTİSİ BURADA DEĞİL,
-- ŞEMADA: `ON CONFLICT DO NOTHING`, migration 00011'deki kısmi unique
-- indeksin (açık + otomatik + monitor başına tek) hakemliğine güveniyor.
-- "Önce SELECT et, açık yoksa yaz" deseni iki eş zamanlı worker kopyasında
-- (3.1'in ölçeklenme hedefi) iki incident açardı.
--
-- Hiç satır dönmemesi bir hata değil, bir CEVAP: "bu monitor için zaten açık
-- bir incident var" (Go tarafında pgx.ErrNoRows olarak okunuyor).
WITH streak AS (
    SELECT min(recent.checked_at) AS started_at
    FROM (
        SELECT c.checked_at
        FROM check_results c
        WHERE c.monitor_id = sqlc.arg(monitor_id)
          AND c.triggered_by = 'scheduled'
          AND c.checked_at <= sqlc.arg(checked_at)
        ORDER BY c.checked_at DESC
        LIMIT sqlc.arg(streak_length)::integer
    ) recent
),
created AS (
    INSERT INTO incidents (
        org_id, title, status, impact,
        auto_monitor_id, is_auto_created, started_at
    )
    SELECT
        -- ORG_ID UYGULAMADAN GELMİYOR: monitor'ün organizasyonu tek doğruluk
        -- kaynağı olarak monitors tablosunda duruyor.
        m.org_id,
        sqlc.arg(title)::text,
        'investigating',
        sqlc.arg(impact)::incident_impact,
        m.id,
        true,
        -- Ham veri budanmışsa (3.4) veya seri hiç okunamazsa tespit anına
        -- düşülüyor: incident'sız kalmaktansa birkaç dakika kısa görünmesi.
        coalesce(streak.started_at, sqlc.arg(checked_at))
    FROM monitors m
    CROSS JOIN streak
    WHERE m.id = sqlc.arg(monitor_id)
    ON CONFLICT (auto_monitor_id) WHERE resolved_at IS NULL DO NOTHING
    RETURNING *
),
linked AS (
    -- Otomatik incident'ta bağ HEP TEK SATIR (0.1 §5). Aynı ifadenin içinde
    -- yazılıyor: ayrı bir çağrı olsaydı aradaki bir hata, hiçbir servise bağlı
    -- olmayan bir incident bırakırdı.
    INSERT INTO incident_monitors (incident_id, monitor_id)
    SELECT created.id, created.auto_monitor_id FROM created
    RETURNING incident_id
),
noted AS (
    -- TIMELINE'IN İLK SATIRI (4.2): incident'ı kimin/neyin açtığı geçmişin
    -- içinde dursun, yalnızca worker logunda değil. Yazar NULL = sistem.
    INSERT INTO incident_updates (incident_id, is_system, status, body)
    SELECT created.id, true, created.status, sqlc.arg(update_body)::text FROM created
    RETURNING id
)
SELECT created.* FROM created;

-- name: ResolveAutoIncident :one
-- Monitor kesintiden çıktığında AÇIK otomatik incident'ı kapatır.
--
-- Simetrik karar: kapanış anı da eşiğin dolduğu an değil, KURTARMA SERİSİNİN
-- İLK BAŞARILI kontrolü — servis o anda cevap vermeye başlamıştı, biz yalnızca
-- emin olmak için birkaç kontrol daha bekledik (3.3'ün kurtarma eşiği).
--
-- `greatest(..., started_at)`: şemadaki "resolved_at >= started_at" kısıtına
-- karşı savunma. Seri sorgusu (ham veri budanmışsa) başlangıçtan eski bir
-- değer üretemez.
--
-- Satır dönmemesi yine bir hata değil: açık otomatik incident yoksa (elle
-- kapatılmış olabilir) yapacak iş de yok.
--
-- Parametrelerdeki AÇIK CAST'ler (`::uuid`, `::timestamptz`) bir üslup tercihi
-- değil: karşılaştıkları sütunlar NULL olabildiği için sqlc onları da "NULL
-- olabilir" sayıp işaretçi tipler üretiyordu — çağıran "hangi monitor" sorusunu
-- asla belirsiz sormuyor.
WITH streak AS (
    SELECT min(recent.checked_at) AS recovered_at
    FROM (
        SELECT c.checked_at
        FROM check_results c
        WHERE c.monitor_id = sqlc.arg(monitor_id)::uuid
          AND c.triggered_by = 'scheduled'
          AND c.checked_at <= sqlc.arg(checked_at)::timestamptz
        ORDER BY c.checked_at DESC
        LIMIT sqlc.arg(streak_length)::integer
    ) recent
)
, resolved AS (
    UPDATE incidents SET
        status      = 'resolved',
        resolved_at = greatest(
            coalesce((SELECT streak.recovered_at FROM streak), sqlc.arg(checked_at)::timestamptz),
            incidents.started_at
        ),
        updated_at  = now()
    WHERE incidents.auto_monitor_id = sqlc.arg(monitor_id)::uuid
      AND incidents.resolved_at IS NULL
    RETURNING *
),
noted AS (
    -- Timeline'ın kapanış satırı (4.2), yine sistem yazarıyla.
    INSERT INTO incident_updates (incident_id, is_system, status, body)
    SELECT resolved.id, true, resolved.status, sqlc.arg(update_body)::text FROM resolved
    RETURNING id
)
SELECT resolved.* FROM resolved;

-- --- Org-scoped okuma/yazma uçları (Faz 4.2) --------------------------------
--
-- Buradan aşağısı api'nin çağırdığı sorgular ve HEPSİ org_id ile sınırlı — tek
-- satırlık okuma dahil (2.1'in kuralı). Üyelik zaten middleware'de doğrulanmış
-- oluyor, yani koşul teoride fazladan; yine de duruyor, çünkü izolasyonu tek
-- bir katmana emanet etmek ileride yanlış bağlanan bir handler'da başka
-- organizasyonun kesinti geçmişini döndürürdü.

-- name: ListIncidentsForOrg :many
-- Incident listesi, yeniden eskiye. Filtre iki bayrakla geliyor:
--   açık      → open_only
--   kapanmış  → resolved_only
--   hepsi     → ikisi de false
-- Üç ayrı sorgu yazmak aynı SELECT'i üç kez kopyalamak olurdu; bayraklar
-- sabit olduğu için planlayıcı açık listede kısmi indeksi
-- (incidents_org_open_idx) kullanabiliyor.
--
-- `id` sıranın son hakemi: aynı ana düşen iki incident'ın sırası
-- deterministik olsun (sayfalama ileride buna dayanacak).
SELECT * FROM incidents
WHERE org_id = sqlc.arg(org_id)
  AND (NOT sqlc.arg(open_only)::boolean     OR resolved_at IS NULL)
  AND (NOT sqlc.arg(resolved_only)::boolean OR resolved_at IS NOT NULL)
ORDER BY started_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetIncidentForOrg :one
SELECT * FROM incidents
WHERE id = sqlc.arg(incident_id) AND org_id = sqlc.arg(org_id);

-- name: ListIncidentMonitors :many
-- Birden fazla incident'ın etkilediği servisler TEK sorguda: liste ucu N
-- incident için N sorgu çalıştırmasın (klasik N+1). Uygulama satırları
-- incident_id'ye göre grupluyor.
--
-- MONITOR ADI BURADAN GELİYOR, incident satırından değil: ad değiştiğinde
-- geçmiş incident'lar da yeni adı gösteriyor (kopyalansaydı iki isim arasında
-- sessiz bir ayrışma doğardı). Monitor SİLİNDİYSE bağ satırı da düşmüş olur
-- (0.1 §7) — incident etkilenen servissiz görünür, ki gerçek de bu.
SELECT im.incident_id, m.id AS monitor_id, m.name, m.current_status
FROM incident_monitors im
JOIN monitors m ON m.id = im.monitor_id
JOIN incidents i ON i.id = im.incident_id
WHERE i.org_id = sqlc.arg(org_id)
  AND im.incident_id = ANY(sqlc.arg(incident_ids)::uuid[])
ORDER BY m.name, m.id;

-- name: ListIncidentUpdates :many
-- Timeline KRONOLOJİK (eskiden yeniye): bir kesintinin anlatısı yukarıdan
-- aşağıya okunur — check geçmişinin (2.3) tersine, orada "son ne oldu"
-- sorulur, burada "ne yaşandı".
SELECT u.*
FROM incident_updates u
JOIN incidents i ON i.id = u.incident_id
WHERE u.incident_id = sqlc.arg(incident_id) AND i.org_id = sqlc.arg(org_id)
ORDER BY u.created_at, u.id;

-- name: AddIncidentUpdate :one
-- Timeline'a bir güncelleme ekler ve incident'ın DURUMUNU o güncellemeye
-- taşır — tek ifadede.
--
-- İkisi ayrı çağrı olsaydı aradaki bir hata, "timeline'da çözüldü yazıyor ama
-- incident hâlâ açık" gibi kendi kendisiyle çelişen bir kayıt bırakırdı
-- (3.3'ün `ApplyCheckOutcome`'ındaki aynı disiplin: durum ve defter tek
-- yazmada).
--
-- `resolved_at` yalnızca 'resolved' güncellemesinde doluyor; şemadaki
-- "(status = 'resolved') = (resolved_at IS NOT NULL)" kısıtı son hakem.
-- Kapanmış bir incident'a yazılamıyor (`resolved_at IS NULL` koşulu): yeniden
-- açma bilinçli olarak yok — kesinti geçmişi geriye dönük değişmemeli, yeni
-- bir sorun yeni bir incident.
WITH target AS (
    SELECT i.id
    FROM incidents i
    WHERE i.id = sqlc.arg(incident_id) AND i.org_id = sqlc.arg(org_id)
      AND i.resolved_at IS NULL
    FOR UPDATE
),
inserted AS (
    INSERT INTO incident_updates (incident_id, author_id, is_system, status, body)
    SELECT target.id, sqlc.arg(author_id)::uuid, false,
           sqlc.arg(status)::incident_status, sqlc.arg(body)::text
    FROM target
    RETURNING *
),
touched AS (
    UPDATE incidents SET
        status      = inserted.status,
        resolved_at = CASE WHEN inserted.status = 'resolved' THEN now() END,
        updated_at  = now()
    FROM inserted
    WHERE incidents.id = inserted.incident_id
    RETURNING incidents.id
)
SELECT inserted.* FROM inserted;

-- name: CreateIncident :one
-- MANUEL incident (4.4): bir admin açıyor, birden fazla servisi kapsayabiliyor.
--
-- `auto_monitor_id` NULL kalıyor — yani 4.1'in kısmi unique indeksi bu kaydı
-- kısıtlamıyor: bir org aynı anda istediği kadar manuel incident açabilir.
-- `started_at` sunucudan (`now()`): kullanıcının seçtiği bir başlangıç, kesinti
-- geçmişini elle yazmanın kapısı olurdu (4.3'ün "geçmişe dönük bakım yok"
-- kararıyla aynı gerekçe).
INSERT INTO incidents (org_id, title, status, impact, is_auto_created, started_at)
VALUES (sqlc.arg(org_id), sqlc.arg(title), 'investigating', sqlc.arg(impact), false, now())
RETURNING *;

-- name: AddIncidentMonitors :execrows
-- Incident'a servis bağlar — YALNIZCA AYNI ORGANİZASYONUN monitor'lerini
-- (maintenance.sql'deki aynı desen). Yazılan satır sayısı dönüyor; çağıran
-- istenenle karşılaştırıp eksikse işlemi geri alıyor.
INSERT INTO incident_monitors (incident_id, monitor_id)
SELECT sqlc.arg(incident_id), m.id
FROM monitors m
WHERE m.id = ANY(sqlc.arg(monitor_ids)::uuid[])
  AND m.org_id = sqlc.arg(org_id);
