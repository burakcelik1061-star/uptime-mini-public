import type { CheckResult } from '@/lib/api-types';
import styles from './ResponseTimeChart.module.css';

/**
 * Yanıt süresi grafiği (2.4).
 *
 * Grafik kütüphanesi YOK: bu projenin görsel işi (bu grafik, 5.2'nin 90
 * günlük uptime barları, sparkline) zaten custom yazılacak; bir kütüphane
 * kendi renk/tipografi dünyasını getirir ve token disiplinini (0.2) bozar.
 * Burada gereken şey birkaç dikdörtgen — SVG onu doğrudan veriyor.
 *
 * ÖLÇÜMSÜZ KONTROLLER GÖRÜNMEZ DEĞİL: cevap alınamayan bir kontrolün süresi
 * yok, ama grafikte tam yükseklikte ve DESENLİ bir bar olarak duruyor.
 * Atlanmış olsalardı kesinti anları grafikte "boşluk" değil "hiç olmamış"
 * gibi görünürdü.
 */

interface Props {
  /** En yeniden eskiye sıralı kontroller (uç böyle döndürüyor). */
  checks: CheckResult[];
}

const CHART_HEIGHT = 120;
const BAR_GAP = 1;

/**
 * Başarısızlık ölçüte göre belirlenir: SONUÇ, süre değil.
 *
 * İlk yazımda "süresi yok = başarısız" varsayılmıştı; bağlantısı reddedilen
 * bir kontrolün hataya kadar geçen süresi ölçülüp saklandığı için (1 ms'nin
 * altı, yani 0 ms) o satır grafikte BAŞARILI bar gibi çizildi. Tarayıcıdan
 * bakınca yakalandı.
 */
function isFailed(check: CheckResult): boolean {
  return check.outcome === 'failure';
}

export function ResponseTimeChart({ checks }: Props) {
  if (checks.length === 0) {
    return (
      <p className={styles.empty}>
        Bu aralıkta kontrol yok. “Şimdi kontrol et” ile bir ölçüm
        çalıştırabilirsiniz.
      </p>
    );
  }

  // Grafik SOLDAN SAĞA zaman akışı: liste yeniden eskiye geldiği için
  // ters çevriliyor.
  const ordered = [...checks].reverse();

  // ÖLÇEK YALNIZCA BAŞARILI/YAVAŞ kontrollerden: başarısız bir kontrolün
  // süresi (hataya kadar geçen zaman) bir yanıt süresi DEĞİL, onu ölçeğe
  // katmak grafiği yanıltırdı.
  const measured = ordered.filter((check) => !isFailed(check));
  const maxMs = measured.reduce(
    (max, check) => Math.max(max, check.responseTimeMs ?? 0),
    0,
  );
  // Hepsi 0 ms ise (yerel hedef) bar yüksekliği hesabı sıfıra bölmesin.
  const scaleMax = Math.max(maxMs, 1);

  const barWidth = 100 / ordered.length;

  return (
    <figure className={styles.figure}>
      <figcaption className={styles.caption}>
        Yanıt süresi · en yüksek {maxMs} ms
      </figcaption>

      <svg
        className={styles.chart}
        viewBox={`0 0 100 ${CHART_HEIGHT}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`${ordered.length} kontrolün yanıt süresi grafiği, en yüksek ${maxMs} milisaniye`}
      >
        <defs>
          {/* Desen, rengi görmeyen kullanıcı için ikinci sinyal: başarısız
              kontroller taralı. */}
          <pattern
            id="failure-hatch"
            width="4"
            height="4"
            patternUnits="userSpaceOnUse"
            patternTransform="rotate(45)"
          >
            <rect width="4" height="4" className={styles.failureBase} />
            <line x1="0" y1="0" x2="0" y2="4" className={styles.failureLine} />
          </pattern>
        </defs>

        {ordered.map((check, index) => {
          const failed = isFailed(check);
          const ratio = failed ? 1 : (check.responseTimeMs ?? 0) / scaleMax;
          // Ölçülen her kontrol en az 2 birim yükseklikte: 0 ms'lik bir
          // kontrol de "oldu" bilgisini taşımalı.
          const height = failed
            ? CHART_HEIGHT
            : Math.max(ratio * CHART_HEIGHT, 2);

          return (
            <rect
              key={check.id}
              x={index * barWidth}
              y={CHART_HEIGHT - height}
              width={Math.max(barWidth - BAR_GAP, 0.5)}
              height={height}
              className={failed ? undefined : styles[`bar-${check.outcome}`]}
              fill={failed ? 'url(#failure-hatch)' : undefined}
            >
              {/* Tarayıcı ipucu: her barın ne olduğu okunabilsin. */}
              <title>
                {new Date(check.checkedAt).toLocaleString('tr-TR')} ·{' '}
                {failed
                  ? `başarısız (${check.errorType ?? 'bilinmiyor'})`
                  : `${check.responseTimeMs ?? '?'} ms`}
              </title>
            </rect>
          );
        })}
      </svg>

      <ul className={styles.legend}>
        <li className={styles.legendItem}>
          <span className={`${styles.swatch} ${styles['bar-success']}`} />
          Başarılı
        </li>
        <li className={styles.legendItem}>
          <span className={`${styles.swatch} ${styles['bar-degraded']}`} />
          Yavaş
        </li>
        <li className={styles.legendItem}>
          <span className={`${styles.swatch} ${styles.swatchFailure}`} />
          Başarısız (ölçüm yok — tam yükseklik, taralı)
        </li>
      </ul>
    </figure>
  );
}
