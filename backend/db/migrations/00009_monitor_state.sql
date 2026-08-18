-- +goose Up
-- Durum makinesinin alanları (Faz 3.3).
--
-- 2.1'in kuralı: sütun, YAZMA YOLUNA sahip olan adımda gelir. `current_status`
-- 2.1'de gelmişti (liste ilk günden durum göstermek zorundaydı) ama doğruluk
-- kaynağı hep bu adımda tanımlanan makine olacaktı; sayaçlar ve eşikler de
-- ancak burada anlam kazanıyor.
ALTER TABLE monitors
    -- Ardışık sayaçlar: durum kararının hafızası. Denormalize tutuluyorlar
    -- (0.1 §5) çünkü alternatif, her kontrolde son N check_result satırını
    -- okumaktı — kontrol başına fazladan bir zaman serisi sorgusu.
    ADD COLUMN consecutive_failures  integer NOT NULL DEFAULT 0
                    CHECK (consecutive_failures >= 0),
    ADD COLUMN consecutive_successes integer NOT NULL DEFAULT 0
                    CHECK (consecutive_successes >= 0),

    -- FLAPPING ÖNLEME EŞİKLERİ, monitor bazında ayarlanabilir.
    --
    -- Varsayılan 3/2 asimetrik ve bilinçli: kesinti İLAN ETMEK pahalı (yanlış
    -- alarm, gereksiz incident), kesintinin BİTTİĞİNİ söylemek ucuz. Üst sınır
    -- 10 — 60 sn aralıklı bir monitor'de 10 ardışık hata, kesintinin 10 dakika
    -- boyunca fark edilmemesi demek; daha büyüğü "izleme" olmaktan çıkardı.
    ADD COLUMN failure_threshold  integer NOT NULL DEFAULT 3
                    CHECK (failure_threshold BETWEEN 1 AND 10),
    ADD COLUMN recovery_threshold integer NOT NULL DEFAULT 2
                    CHECK (recovery_threshold BETWEEN 1 AND 10),

    -- Durumun EN SON DEĞİŞTİĞİ an. "Ne zamandır down?" sorusunun cevabı;
    -- 4.1'in incident başlangıç zamanı da buradan türeyecek.
    ADD COLUMN status_changed_at timestamptz NOT NULL DEFAULT now();

-- Mevcut satırlarda durum hiç değişmedi (hepsi oluşturulduğu durumda):
-- status_changed_at = created_at. `now()` bırakmak, aylardır duran bir
-- monitor'ün durumunun "az önce değişmiş" görünmesi olurdu.
UPDATE monitors SET status_changed_at = created_at;

-- +goose Down
ALTER TABLE monitors
    DROP COLUMN IF EXISTS status_changed_at,
    DROP COLUMN IF EXISTS recovery_threshold,
    DROP COLUMN IF EXISTS failure_threshold,
    DROP COLUMN IF EXISTS consecutive_successes,
    DROP COLUMN IF EXISTS consecutive_failures;
