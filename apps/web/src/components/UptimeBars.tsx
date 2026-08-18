import type { UptimeDay } from '@/lib/api-types';
import styles from './UptimeBars.module.css';

/**
 * 90 GÜNLÜK UPTIME GEÇMİŞİ (Faz 5.2) — Instatus/BetterStack tarzı bar şeridi.
 *
 * Veri 3.4'ün GÜNLÜK ÖZETLERİNDEN geliyor (ham kontrol satırlarından değil):
 * ham veri 30 gün sonra budanıyor, yani 90 günün ilk 60'ı yalnızca özetlerde
 * yaşıyor (0.1 §6). Yüzdeyi de sunucu hesaplıyor — bileşen yalnızca çiziyor.
 *
 * KÜTÜPHANE YOK (2.4'ün kararının devamı): bir grafik kütüphanesi kendi
 * renk/tipografi dünyasını getirip token disiplinini bozardı ve burada çizilen
 * şey zaten 90 dikdörtgen.
 *
 * ÜÇ SİNYAL, ÜÇÜ DE RENKTEN BAĞIMSIZ (0.2'nin şartı):
 *   1. Renk — durum token'ları.
 *   2. DESEN — "veri yok" ve "bakım" günleri taralı (`--u-pattern-*`).
 *      Bunlar en kritik iki durum: veri yok günü yeşil görünmemeli.
 *   3. METİN — bar üstünde tooltip, ayrıca şeridin `aria-label`ında sorunlu
 *      günler tek tek yazılı.
 *
 * ERİŞİLEBİLİRLİK KARARI: barlar TEK TEK ODAKLANABİLİR DEĞİL. 90 bar × servis
 * sayısı kadar tab durağı, klavye kullanıcısı için sayfayı geçilmez yapardı.
 * Bunun yerine şerit tek bir `role="img"` ve etiketinde bilginin ÖZÜ var:
 * genel oran + sorunlu günlerin listesi. Fare tooltip'i bu etiketin üstüne
 * eklenen bir kolaylık, tek kaynağı değil.
 */

const STATE_CLASS: Record<UptimeDay['state'], string> = {
  up: styles.up,
  partial: styles.partial,
  down: styles.down,
  maintenance: styles.maintenance,
  no_data: styles.noData,
};

/** Yüzdeyi iki basamakla, Türkçe ondalık ayracıyla yazar. */
function formatPercent(value: number): string {
  return `%${value.toFixed(2).replace('.', ',')}`;
}

function formatDay(date: string): string {
  // Gün UTC takvim günü; `T00:00:00Z` eklemeden ayrıştırmak tarayıcıya bırakılan
  // bir yorum olurdu.
  return new Date(`${date}T00:00:00Z`).toLocaleDateString('tr-TR', {
    day: 'numeric',
    month: 'long',
    year: 'numeric',
    timeZone: 'UTC',
  });
}

/** Bir günün tek cümlelik anlatımı (hem tooltip hem etiket bunu kullanıyor). */
function describeDay(day: UptimeDay): string {
  switch (day.state) {
    case 'no_data':
      // "Veri yok" ile "%100" ayrımı burada da korunuyor.
      return 'veri yok';
    case 'maintenance':
      return 'gün boyunca planlı bakım';
    default: {
      const percent =
        day.uptimePercent === null
          ? 'veri yok'
          : formatPercent(day.uptimePercent);

      return day.maintenanceChecks > 0
        ? `${percent} · ${day.maintenanceChecks} kontrol planlı bakımda`
        : percent;
    }
  }
}

/**
 * Şeridin metin özeti.
 *
 * `headline` alt şeritte görünüyor, `label` ise `role="img"`in etiketi: sorunlu
 * günleri tek tek sayıyor. Ekran okuyucu kullanan biri, 90 barın üzerinde
 * gezinmek zorunda kalmadan aynı bilgiyi alıyor.
 *
 * ORTALAMA YALNIZCA ÖLÇÜLMÜŞ GÜNLERDEN: "veri yok" günlerini %0 saymak
 * uydurulmuş bir kesinti, %100 saymak uydurulmuş bir başarı olurdu (0.1'in
 * "hiç kontrol yoksa uptime null" kuralı gün seviyesinde de geçerli).
 */
function summarize(days: UptimeDay[]): { headline: string; label: string } {
  const measured = days.filter((day) => day.uptimePercent !== null);

  const average =
    measured.length === 0
      ? null
      : measured.reduce((total, day) => total + (day.uptimePercent ?? 0), 0) /
        measured.length;

  // "Veri yok" günleri TEK TEK sayılmıyor, SAYILIYOR: yeni bir monitor'ün 80
  // boş günü etiketi okunmaz hâle getirirdi. Gerçekten anlatılması gereken şey
  // kesinti yaşanan günler.
  const missing = days.filter((day) => day.state === 'no_data');
  const problems = days.filter(
    (day) => day.state === 'partial' || day.state === 'down',
  );

  const headline =
    average === null
      ? `Son ${days.length} günde hiç ölçüm yok`
      : `Son ${days.length} gün · ortalama ${formatPercent(average)}`;

  const parts: string[] = [];

  if (problems.length === 0) {
    parts.push('Kesinti yaşanan gün yok.');
  } else {
    const listed = problems.slice(0, MAX_LISTED_PROBLEM_DAYS);

    parts.push(
      `${listed
        .map((day) => `${formatDay(day.date)}: ${describeDay(day)}`)
        .join('. ')}.`,
    );

    if (problems.length > listed.length) {
      parts.push(`Ve ${problems.length - listed.length} gün daha kesintili.`);
    }
  }

  if (missing.length > 0) {
    parts.push(`${missing.length} gün için ölçüm yok.`);
  }

  return { headline, label: `${headline}. ${parts.join(' ')}` };
}

/** Etikette adı geçen kesinti günü sayısının üst sınırı. */
const MAX_LISTED_PROBLEM_DAYS = 10;

export function UptimeBars({ days }: { days: UptimeDay[] }) {
  if (days.length === 0) return null;

  const first = days[0];
  const last = days[days.length - 1];
  const { headline, label } = summarize(days);

  return (
    <div className={styles.wrapper}>
      {/* Şerit TEK bir görsel: içindeki 90 kutu ekran okuyucuya tek tek
          okunmuyor, özet etikette yazılı. */}
      <div className={styles.strip} role="img" aria-label={label}>
        {days.map((day) => (
          <span
            key={day.date}
            className={`${styles.day} ${STATE_CLASS[day.state]}`}
          >
            <span className={styles.tooltip} aria-hidden="true">
              <span className={styles.tooltipDay}>{formatDay(day.date)}</span>
              <span className={styles.tooltipValue}>{describeDay(day)}</span>

              {day.incidents.length > 0 && (
                <span className={styles.tooltipIncidents}>
                  {day.incidents.map((incident) => (
                    <span key={incident.id} className={styles.tooltipIncident}>
                      {incident.title}
                      {incident.isOpen ? ' (sürüyor)' : ''}
                    </span>
                  ))}
                </span>
              )}
            </span>
          </span>
        ))}
      </div>

      <div className={styles.legend}>
        <span className={`${styles.axis} tabularNumbers`}>
          {formatDay(first.date)}
        </span>
        <span className={styles.summary}>{headline}</span>
        <span className={`${styles.axis} tabularNumbers`}>
          {formatDay(last.date)}
        </span>
      </div>
    </div>
  );
}
