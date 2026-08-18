-- Scheduler sorguları (Faz 3.2).
--
-- Bu dosyadaki sorgular ORG-SCOPED DEĞİL — monitors.sql'deki her sorgunun
-- `org_id` şart koşmasının aksine. Sebep: buradaki çağıran bir kullanıcı adına
-- iş yapmıyor, worker sürecidir ve TÜM kiracıların monitor'lerini zamanlıyor.
-- Org izolasyonu kullanıcının VERİYE ERİŞİMİ hakkında bir kural; zamanlayıcı
-- veriyi kimseye göstermiyor. Yine de yazma yüzeyi dar: bu sorgular yalnızca
-- zamanlama sütunlarına dokunuyor (yapılandırmaya ve `current_status`'a asla).

-- name: ClaimDueMonitors :many
-- Sırası gelen monitor'leri ATOMİK olarak sahiplenir.
--
-- Tek bir ifade, üç garanti:
--
--  1. AYNI MONITOR EŞZAMANLI İKİ KEZ KONTROL EDİLMEZ. `FOR UPDATE` satırı
--     kilitliyor, `SKIP LOCKED` ise başka bir worker'ın kilitlediği satırı
--     BEKLEMEK yerine atlıyor (beklemek, iki worker'ı sıraya dizip
--     paralelliği yok ederdi). Kilit tutulurken aynı ifade `next_check_at`'i
--     ileri alıyor, yani kilit bırakıldığında satır artık "sırası gelmiş"
--     değil — ikinci worker onu görse bile almıyor.
--  2. ÇOK WORKER'DA DOĞRU. Sahiplenme kararı uygulamada değil veritabanında
--     veriliyor ("önce SELECT et, sonra UPDATE et" iki isteğin aynı satırı
--     almasına açıktı). Kopya sayısını arttırmak koordinasyon gerektirmiyor.
--  3. RESTART'TA DUPLİKE YOK. Sahiplenilen satırın sırası ileri alındığı için
--     yeniden başlayan bir worker aynı kontrolü tekrar çalıştırmıyor.
--     (Sahiplenilip TAMAMLANAMAYAN kontrol için bkz. ReleaseMonitorClaim.)
--
-- `next_check_at` = now() + aralık: ölçüm HER ZAMAN "kontrol başlangıçları
-- arası en az aralık kadar" oluyor. `next_check_at + aralık` yazılsaydı, uzun
-- süre kapalı kalmış bir worker açıldığında birikmiş turların hepsini birden
-- çalıştırırdı (60 sn aralıklı bir monitor için bir saatlik kesinti = 60
-- kontrolün aynı anda patlaması). Telafi bilinçli olarak YOK.
--
-- `updated_at` KASITLI OLARAK ELLENMİYOR: o sütun "yapılandırma değişti"
-- demek. Zamanlama her kontrolde ilerliyor; ellenirse kullanıcının hiç
-- dokunmadığı bir monitor sürekli "az önce güncellendi" görünürdü.
WITH due AS (
    SELECT id
    FROM monitors
    WHERE is_active AND next_check_at <= now()
    -- En çok gecikmiş olan önce: yük altında bazı monitor'ler sürekli sona
    -- kalmasın (adil sıra).
    ORDER BY next_check_at
    FOR UPDATE SKIP LOCKED
    LIMIT sqlc.arg(row_limit)
)
UPDATE monitors m
SET next_check_at = now() + (m.check_interval_seconds * interval '1 second')
FROM due
WHERE m.id = due.id
RETURNING m.*;

-- name: ApplyCheckOutcome :exec
-- Kontrol GERÇEKTEN çalıştıktan sonra monitor satırındaki defteri günceller:
-- son kontrol zamanı + durum makinesinin (3.3) ürettiği durum ve sayaçlar.
--
-- TEK İFADE: sonuç, sayaçlar ve durum aynı yazmada güncelleniyor. İki ayrı
-- sorgu olsaydı aralarında kalan bir hata, sayacı ilerlemiş ama durumu eski
-- (ya da tersi) bir satır bırakırdı.
--
-- Zaman istemciden (worker'dan) geliyor, `now()` değil: yazılan an, ölçümün
-- alındığı an olmalı — kontrol 10 saniye sürdüyse `now()` 10 saniye ileriyi
-- gösterirdi ve check_results satırındaki `checked_at` ile ayrışırdı.
--
-- `status_changed_at` YALNIZCA durum gerçekten değiştiğinde yazılıyor: her
-- kontrolde tazelenseydi "ne zamandır down?" sorusunun cevabı hep "az önce"
-- olurdu (4.1'in incident başlangıcı da oradan türeyecek).
--
-- `is_active` KOŞULU BİR YARIŞI KAPATIYOR: kontrol sürerken kullanıcı
-- monitor'ü duraklatmış olabilir. Koşul olmasaydı, biten kontrol 'paused'
-- durumunun üzerine 'up' yazardı — kullanıcının az önce verdiği kararı ezmek.
UPDATE monitors
SET last_checked_at       = sqlc.arg(checked_at),
    current_status        = sqlc.arg(status),
    consecutive_failures  = sqlc.arg(consecutive_failures),
    consecutive_successes = sqlc.arg(consecutive_successes),
    status_changed_at     = CASE
                                WHEN current_status IS DISTINCT FROM sqlc.arg(status)
                                THEN sqlc.arg(checked_at)
                                ELSE status_changed_at
                            END
WHERE id = sqlc.arg(id) AND is_active;

-- name: ReleaseMonitorClaim :exec
-- Sahiplenilmiş ama ÇALIŞTIRILAMAMIŞ bir kontrolün sırasını geri verir.
--
-- Ne zaman: `checker.ErrCheckNotRun` — yani ölçüm hiç alınmadı (worker
-- kapanıyor). Sıra geri verilmezse o monitor bir tur (aralığı kadar) atlanmış
-- olurdu; deploy sırasında sessizce körleşen bir monitor demek.
--
-- `is_active` koşulu şemadaki tutarlılık kısıtını koruyor: kontrol sürerken
-- monitor duraklatılmışsa sıra NULL kalmalı, geri verilmemeli.
UPDATE monitors
SET next_check_at = now()
WHERE id = sqlc.arg(id) AND is_active;
