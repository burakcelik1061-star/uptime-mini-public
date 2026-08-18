import type { CheckResult } from '@/lib/api-types';
import styles from './CheckTable.module.css';

/**
 * Son kontroller listesi (2.4).
 *
 * Sonuç renk + METİN ile gösteriliyor; ayrıca manuel kontroller ayrı bir
 * etiket taşıyor — kullanıcı "bu kesinti kaydını ben mi ürettim" sorusunu
 * cevaplayabilsin (2.3'ün `triggered_by` kararının arayüzdeki karşılığı).
 */

const OUTCOME_LABELS: Record<CheckResult['outcome'], string> = {
  success: 'Başarılı',
  degraded: 'Yavaş',
  failure: 'Başarısız',
};

const ERROR_LABELS: Record<string, string> = {
  timeout: 'zaman aşımı',
  dns: 'alan adı çözülemedi',
  connection_refused: 'bağlantı reddedildi',
  tls: 'TLS hatası',
  unexpected_status: 'beklenmeyen status',
  blocked_url: 'adres engellendi',
  unknown: 'bilinmeyen hata',
};

function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('tr-TR', {
    day: '2-digit',
    month: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

/**
 * Hata hücresi: tipin insan karşılığı + (varsa) sunucunun mesajı.
 *
 * Mesaj tipin etiketiyle AYNIYSA tekrar edilmiyor — "bağlantı reddedildi"
 * satırı iki kez yazmak bilgi eklemiyor.
 */
function ErrorCell({
  errorType,
  errorMessage,
}: {
  errorType: string | null;
  errorMessage: string | null;
}) {
  if (!errorType) return <>—</>;

  const label = ERROR_LABELS[errorType] ?? errorType;
  const detail =
    errorMessage && errorMessage.toLowerCase() !== label.toLowerCase()
      ? errorMessage
      : null;

  return (
    <>
      {label}
      {detail && <span className={styles.errorMessage}>{detail}</span>}
    </>
  );
}

export function CheckTable({ checks }: { checks: CheckResult[] }) {
  if (checks.length === 0) {
    return <p className={styles.empty}>Bu aralıkta kontrol yok.</p>;
  }

  return (
    <div className={styles.wrapper}>
      <table className={styles.table}>
        <caption className={styles.caption}>Son kontroller</caption>
        <thead>
          <tr>
            <th scope="col">Zaman</th>
            <th scope="col">Sonuç</th>
            <th scope="col">HTTP</th>
            <th scope="col">Süre</th>
            <th scope="col">Kaynak</th>
            <th scope="col">Hata</th>
          </tr>
        </thead>
        <tbody>
          {checks.map((check) => (
            <tr key={check.id}>
              <td className="tabularNumbers">{formatTime(check.checkedAt)}</td>
              <td>
                <span className={`${styles.outcome} ${styles[check.outcome]}`}>
                  {OUTCOME_LABELS[check.outcome]}
                </span>
              </td>
              {/* Cevap alınamadıysa "—": 0 gibi bir değer uydurulmuyor. */}
              <td className="tabularNumbers">{check.httpStatus ?? '—'}</td>
              <td className="tabularNumbers">
                {check.responseTimeMs === null
                  ? '—'
                  : `${check.responseTimeMs} ms`}
              </td>
              <td>
                {check.triggeredBy === 'manual' ? (
                  <span className={styles.manual}>manuel</span>
                ) : (
                  <span className={styles.scheduled}>zamanlanmış</span>
                )}
              </td>
              <td className={styles.error}>
                <ErrorCell
                  errorType={check.errorType}
                  errorMessage={check.errorMessage}
                />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
