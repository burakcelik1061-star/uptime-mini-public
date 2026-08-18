-- Bakım penceresi sorguları (Faz 4.3).
--
-- Org-scoped uçlar org_id ile sınırlı (2.1'in kuralı). Tek istisna
-- `MonitorInMaintenanceAt`: onu WORKER çağırıyor ve sorunun içinde org yok —
-- monitor'ün hangi organizasyona ait olduğu zaten bağ tablosunun ucunda
-- (rollups.sql ile aynı desen).

-- name: MonitorInMaintenanceAt :one
-- 4.1'in incident kapısı: "bu monitor şu anda planlı bakımda mı?"
--
-- Yarı açık aralık [starts_at, ends_at): pencerenin bittiği saniyede kontrol
-- artık bakımda değil.
--
-- Yalnızca DURUM DEĞİŞİMLERİNDE çağrılıyor (her kontrolde değil): bir monitor
-- `down`a geçtiğinde. Sıcak yola fazladan bir sorgu eklemenin gereği yok.
SELECT EXISTS (
    SELECT 1
    FROM maintenance_window_monitors mwm
    JOIN maintenance_windows w ON w.id = mwm.window_id
    WHERE mwm.monitor_id = sqlc.arg(monitor_id)
      AND sqlc.arg(at)::timestamptz >= w.starts_at
      AND sqlc.arg(at)::timestamptz <  w.ends_at
) AS in_maintenance;

-- name: CreateMaintenanceWindow :one
INSERT INTO maintenance_windows (org_id, title, description, starts_at, ends_at)
VALUES (
    sqlc.arg(org_id), sqlc.arg(title), sqlc.narg(description),
    sqlc.arg(starts_at), sqlc.arg(ends_at)
)
RETURNING *;

-- name: AddMaintenanceWindowMonitors :execrows
-- Pencereye monitor bağlar — YALNIZCA AYNI ORGANİZASYONUN monitor'lerini.
--
-- Filtre bu sorgunun içinde: "önce doğrula, sonra yaz" deseninde araya giren
-- bir silme/taşıma başka organizasyonun monitor'ünü bağlayabilirdi. Kaç satır
-- yazıldığı dönüyor; çağıran istenen sayıyla karşılaştırıp eksikse (yani
-- listede yabancı ya da olmayan bir kimlik varsa) tüm işlemi geri alıyor.
INSERT INTO maintenance_window_monitors (window_id, monitor_id)
SELECT sqlc.arg(window_id), m.id
FROM monitors m
WHERE m.id = ANY(sqlc.arg(monitor_ids)::uuid[])
  AND m.org_id = sqlc.arg(org_id);

-- name: ListMaintenanceWindowsForOrg :many
-- Pencereler, yeniden eskiye. "Aktif / yaklaşan / geçmiş" ayrımı SORGUDA YOK:
-- üçü de `starts_at`/`ends_at` ile şimdiden türetilebiliyor ve sunucunun
-- "şimdi"si ile istemcininki arasındaki fark yüzünden bir pencere iki farklı
-- kutuya düşmesin (2.4'ün "aralık zorunlu" kararıyla aynı çekince).
SELECT * FROM maintenance_windows
WHERE org_id = sqlc.arg(org_id)
ORDER BY starts_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetMaintenanceWindowForOrg :one
SELECT * FROM maintenance_windows
WHERE id = sqlc.arg(window_id) AND org_id = sqlc.arg(org_id);

-- name: ListMaintenanceWindowMonitors :many
-- Birden fazla pencerenin monitor'leri TEK sorguda (N+1 yok, incidents.sql'in
-- `ListIncidentMonitors`'ıyla aynı desen). Ad canlı kayıttan geliyor.
SELECT mwm.window_id, m.id AS monitor_id, m.name, m.current_status
FROM maintenance_window_monitors mwm
JOIN monitors m ON m.id = mwm.monitor_id
JOIN maintenance_windows w ON w.id = mwm.window_id
WHERE w.org_id = sqlc.arg(org_id)
  AND mwm.window_id = ANY(sqlc.arg(window_ids)::uuid[])
ORDER BY m.name, m.id;

-- name: DeleteUpcomingMaintenanceWindow :execrows
-- Yalnızca HENÜZ BAŞLAMAMIŞ pencere silinebiliyor.
--
-- Başlamış bir pencereyi silmek, geçmişi yeniden yazmak demek: o pencereye
-- düşen kontroller `maintenance_checks` olarak zaten özetlenmiş olabilir ve
-- silme sonrası aynı gün için iki farklı uptime cevabı doğardı (3.4'ün
-- toplulaştırması kapanmış kovaları yeniden hesaplamıyor). Aynı gerekçe
-- geçmişe dönük pencere OLUŞTURMAYI da yasaklıyor (bkz. internal/maintenance).
--
-- 0 satır → "yok, başkasının ya da çoktan başlamış"; çağıran hangisi olduğunu
-- ayrıca sorguyla ayırıyor (404 ile 409 farkı için).
DELETE FROM maintenance_windows
WHERE id = sqlc.arg(window_id) AND org_id = sqlc.arg(org_id)
  AND starts_at > now();
